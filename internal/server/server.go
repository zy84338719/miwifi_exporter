// Package server 负责 HTTP 暴露层：/metrics、健康探针与首页。
//
// 底座是 oekit 的 Hertz 集成（middleware 标准栈 + bootstrap 生命周期 +
// bootstrap/healthz 探针），路由本身保持极薄——采集逻辑一律在 internal/collector。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"reflect"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/oekit/bootstrap/healthz"
	ekithertz "github.com/oekit/hertz"
	hertzmetrics "github.com/oekit/hertz/metrics"
	"github.com/oekit/logx"
	mw "github.com/oekit/middleware"
	hmw "github.com/oekit/middleware/hmw"
	oerr "github.com/oekit/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/config"
)

// ServiceName 是日志与 healthz 中使用的服务名。
const ServiceName = "miwifi-exporter"

// contextCollector 是「愿意接收请求 context」的采集器能力。
//
// 刻意定义成接口而不是直接依赖 collector.Collector：
// server 包不应该知道采集器长什么样，只要它能接住 context 就行。
type contextCollector interface {
	SetRequestContext(ctx context.Context)
}

// Options 是 HTTP 服务的构造参数。
type Options struct {
	Config *config.Config
	// Registry 是暴露在 /metrics 上的注册表（包含路由器指标与 exporter 自监控指标）。
	Registry *prometheus.Registry
	// ReadyCheck 可选，用于 /readyz 汇报 exporter 自身是否已就绪。
	ReadyCheck func(context.Context) error
	// RouterStatus 可选：返回逐台路由器的状态结构（可 JSON 序列化），
	// 供 /routers 端点与首页使用。
	//
	// 刻意用 `func() any` 而不是具体的类型：server 层不该知道采集层的
	// 状态结构长什么样，只负责把它呈现出来。
	RouterStatus func() any
	// ContextCollector 可选：实现了 SetRequestContext 的采集器。
	//
	// 注入请求 context 后，Prometheus 到了 scrape_timeout 断开时，
	// exporter 会真的停下来，而不是把十个接口白跑完。
	ContextCollector contextCollector
	// Logger 覆盖默认 logger。
	Logger logx.Logger
	// LogLevel 决定框架日志的级别。
	LogLevel logx.Level
}

// New 构造并装配 Hertz 实例（尚未启动）。
func New(opts Options) (*server.Hertz, error) {
	cfg := opts.Config
	if cfg == nil {
		return nil, oerr.NewBadRequestError("server: Config 不能为空")
	}
	if opts.Registry == nil {
		return nil, oerr.NewBadRequestError("server: Registry 不能为空")
	}

	logger := opts.Logger
	if logger == nil {
		logger = logx.Delegating("server")
	}

	h := server.New(
		server.WithHostPorts(fmt.Sprintf(":%d", cfg.Port)),
		server.WithExitWaitTime(cfg.ShutdownTimeout),
	)
	// Hertz 框架自身的日志（连接异常、路由警告）接回 logx，否则会绕过日志门面
	// 直接打到 stderr，且不带 service 与结构化字段。
	//
	// 这里必须传 slog/std 后端，绝不能传 hlog 背端的 Logger——那会形成
	// adapter → logx → hlog → adapter 的无限递归，直接把进程打到栈溢出。
	hlog.SetLogger(ekithertz.NewLogger(logx.NewStdLogger(ServiceName+"-http", opts.LogLevel)))

	// 标准栈：TraceID → Recovery → Response → AccessLog。
	//
	// 这里没有直接用 mw.DefaultMiddleware，是因为它的 StackConfig.SkipPaths
	// 只作用于 AccessLog，Response 仍会把响应包成 {code,data,message,trace_id}。
	// /metrics 要是被包一层，Prometheus 立刻解析失败。ResponseMiddleware 的
	// WithSkipPaths 才是对响应包装生效的那个开关，探针与首页一并排除。
	// /routers 必须显式排除：它返回的是裸 JSON 数组，
	// 被 ResponseMiddleware 包成 {code,data} 之后 curl -s | jq 就不再好用了。
	skipPaths := []string{cfg.MetricsPath, "/healthz", "/livez", "/readyz", "/routers", "/"}
	// 注意 append(f()..., x) 不是合法 Go 语法（... 只能出现在最后一个实参），
	// 所以这里先取回 hmw 的栈再逐个追加。
	stack := hmw.Middleware()
	stack = append(stack,
		mw.RecoveryMiddleware(),
		mw.ResponseMiddleware(mw.WithSkipPaths(skipPaths...)),
		mw.AccessLogMiddleware(mw.WithAccessLogSkipPaths(skipPaths...)),
	)
	for _, m := range stack {
		h.Use(m)
	}

	// /metrics 走 oekit 的 metrics 适配器：它会把 Accept-Encoding / Accept
	// 透传给 promhttp，gzip 压缩与 OpenMetrics 协商因此能正常工作。
	metricsHandler := hertzmetrics.MetricsHandlerFor(opts.Registry)
	if opts.ContextCollector != nil {
		injector := opts.ContextCollector
		h.GET(cfg.MetricsPath, func(ctx context.Context, c *app.RequestContext) {
			injector.SetRequestContext(ctx)
			metricsHandler(ctx, c)
		})
	} else {
		h.GET(cfg.MetricsPath, metricsHandler)
	}

	h.GET("/healthz", healthHandler)
	h.GET("/readyz", healthHandler)
	// /livez 只看进程存活，恒 200 —— 探活绝不能依赖路由器，否则路由器重启
	// 会连带把 exporter 一起重启。
	h.GET("/livez", func(_ context.Context, c *app.RequestContext) {
		c.Data(200, "application/json; charset=utf-8", []byte(`{"status":"ok"}`))
	})
	h.GET("/", landingHandler(cfg, logger, opts.RouterStatus))
	// /routers：现场排障用。指标能告诉你「出问题了」，但看不出
	// 「是哪台、哪个端点、被谁跳过、为什么」——这个端点补上那段信息。
	if opts.RouterStatus != nil {
		h.GET("/routers", routerStatusHandler(opts.RouterStatus))
	}

	// 把就绪判据挂进 healthz 的全局注册表，/healthz 与 /readyz 都会带上它。
	//
	// healthz 用的是包级 singleton，因此 New 每个进程应当只调一次；
	// 重复调用会让同名 check 被追加多次（测试里会看到 check 数量增长）。
	if opts.ReadyCheck != nil {
		healthz.RegisterCheck(readyCheckName, healthz.Check(opts.ReadyCheck))
	}

	logger.Info("HTTP 服务已装配", "port", cfg.Port, "metrics_path", cfg.MetricsPath)
	return h, nil
}

// readyCheckName 是 readiness 检查在 healthz 注册表里的名字。
const readyCheckName = "exporter"

// healthHandler 处理 /healthz 与 /readyz。
//
// 刻意**不**把「路由器是否可达」做成探针失败：exporter 的价值恰恰在于
// 路由器出问题时仍能把「出问题」这件事暴露成指标。把探针绑到路由器上，
// K8s 会把 Pod 摘出 Service，结果是连告警指标都抓不到了。
// 路由器健康度请看 miwifi_exporter_router_up 与 miwifi_exporter_endpoint_up。
func healthHandler(ctx context.Context, c *app.RequestContext) {
	status, body := healthz.Handle(ctx)
	c.Data(status, "application/json; charset=utf-8", []byte(body))
}

// landingHandler 是首页。
//
// 多路由器之后，首页最有用的信息是「我在采哪几台、各自什么状态」，
// 所以直接渲染成一张表，而不是只丢一个 Metrics 链接。
func landingHandler(cfg *config.Config, logger logx.Logger, status func() any) app.HandlerFunc {
	return func(_ context.Context, c *app.RequestContext) {
		c.Data(consts.StatusOK, "text/html; charset=utf-8", []byte(renderIndex(cfg, status)))
	}
}

func renderIndex(cfg *config.Config, status func() any) string {
	var b strings.Builder
	b.WriteString(`<html><head><title>miwifi-exporter</title>
<meta name="viewport" content="width=device-width,initial-scale=1"></head><body>
<h1>miwifi-exporter</h1>
<p>Prometheus exporter for Xiaomi MiWiFi routers.</p>
<ul>
	<li><a href="` + cfg.MetricsPath + `">Metrics</a></li>`)
	if status != nil {
		b.WriteString(`<li><a href="/routers">Routers</a></li>`)
	}
	b.WriteString(`<li><a href="/healthz">Health</a></li>
	<li><a href="/readyz">Ready</a></li>
	<li><a href="/livez">Live</a></li>
</ul>`)

	if status != nil {
		b.WriteString("<h2>Routers</h2>\n")
		b.WriteString(renderRouterTable(status()))
	}
	b.WriteString("</body></html>")
	return b.String()
}

// renderRouterTable 把逐台状态渲染成表格。
//
// 用反射而不是让 server 依赖 collector 的具体类型：server 是最外层，
// 依赖倒置在这里比少写一个 import 重要。
func renderRouterTable(status any) string {
	v := reflect.ValueOf(status)
	if !v.IsValid() || v.Kind() != reflect.Slice {
		return ""
	}

	cell := func(item reflect.Value, name string) string {
		f := item.FieldByName(name)
		if !f.IsValid() {
			return ""
		}
		switch f.Kind() {
		case reflect.String:
			return html.EscapeString(f.String())
		case reflect.Bool:
			if f.Bool() {
				return "yes"
			}
			return "no"
		case reflect.Int, reflect.Int64:
			return strconv.FormatInt(f.Int(), 10)
		case reflect.Slice:
			parts := make([]string, 0, f.Len())
			for j := 0; j < f.Len(); j++ {
				parts = append(parts, html.EscapeString(f.Index(j).String()))
			}
			return strings.Join(parts, ", ")
		default:
			return ""
		}
	}

	var b strings.Builder
	b.WriteString(`<table border="1" cellpadding="4" cellspacing="0">
<tr><th>host</th><th>ip</th><th>profile</th><th>mode</th><th>ready</th>` +
		`<th>schema</th><th>enabled endpoints</th><th>disabled</th><th>timeouts</th></tr>`)

	for i := 0; i < v.Len(); i++ {
		item := v.Index(i)
		fmt.Fprintf(&b,
			"<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			cell(item, "Host"), cell(item, "IP"), cell(item, "Profile"), cell(item, "Mode"),
			cell(item, "Ready"), cell(item, "SchemaAligned"),
			cell(item, "Endpoints"), cell(item, "Disabled"), cell(item, "Timeouts"))
	}
	b.WriteString("</table>")
	return b.String()
}

// routerStatusHandler 提供 /routers 的 JSON 输出。
func routerStatusHandler(status func() any) app.HandlerFunc {
	return func(_ context.Context, c *app.RequestContext) {
		body, err := json.Marshal(status())
		if err != nil {
			c.Data(consts.StatusInternalServerError, "application/json; charset=utf-8",
				[]byte(`{"error":"marshal router status failed"}`))
			return
		}
		c.Data(consts.StatusOK, "application/json; charset=utf-8", body)
	}
}
