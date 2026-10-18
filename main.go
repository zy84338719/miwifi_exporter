// Command miwifi-exporter 是一个 Prometheus exporter，用于采集小米路由器（MiWiFi）状态。
//
// 采集链路走 oekit（github.com/oekit/*）的模块矩阵：logx 日志门面、pkg/errors 错误、
// concurrency 的并发/重试/熔断/单飞、core/config 配置加载、bootstrap 生命周期、
// middleware 中间件栈、hertz HTTP 运行时。
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/oekit/bootstrap"
	"github.com/oekit/bootstrap/healthz"
	ekithertz "github.com/oekit/hertz"
	"github.com/oekit/logx"
	oerr "github.com/oekit/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/config"
	"github.com/helloworlde/miwifi-exporter/internal/server"
)

// version 由构建时 -ldflags "-X main.version=..." 注入。
var version = "dev"

// buildDate 由构建时 -ldflags "-X main.buildDate=..." 注入。
var buildDate = "unknown"

func main() {
	if err := run(); err != nil {
		// 此时 logx 可能尚未初始化，双写一份到 stderr 保证原因不丢。
		fmt.Fprintf(os.Stderr, "miwifi-exporter 启动失败: %v\n", err)
		logx.Error("启动失败", "err", err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		configFile   = flag.String("config", config.DefaultConfigFile, "配置文件路径（JSON）")
		metricsPath  = flag.String("web.telemetry-path", "", "暴露指标的路径，覆盖 METRICS_PATH")
		namespace    = flag.String("metric.namespace", "", "Prometheus 指标 namespace 前缀，覆盖 METRICS_NAMESPACE")
		collectMode  = flag.String("collect-mode", "", "采集模式 pull（scrape 时实时取数）或 poll（后台轮询缓存）")
		showVersion  = flag.Bool("version", false, "打印版本后退出")
		validateOnly = flag.Bool("validate-config", false, "只校验配置，不启动服务")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("miwifi-exporter %s (%s, built %s)\n", version, runtime.Version(), buildDate)
		return nil
	}

	cfg, err := loadConfig(*configFile, config.Overrides{
		MetricsPath: *metricsPath,
		Namespace:   *namespace,
		CollectMode: *collectMode,
	})
	if err != nil {
		return err
	}
	if err := config.ValidateRouterHost(cfg.RouterHost); err != nil {
		return err
	}

	// ParseLogLevel 对无法识别的字符串返回 nil；此时退回 info，
	// 不让一个拼错的 LOG_LEVEL 换来一个「什么都不打」的进程。
	level := logx.LevelInfo
	if parsed := logx.ParseLogLevel(cfg.LogLevel); parsed != nil {
		level = *parsed
	}
	logx.SetDefault(logx.NewStdLogger(server.ServiceName, level))

	logger := logx.WithService(server.ServiceName)
	healthz.SetCheckTimeout(cfg.RouterTimeout)
	logger.Info("miwifi-exporter 启动中",
		"version", version, "go", runtime.Version(), "build_date", buildDate,
		"mode", cfg.CollectMode, "config", cfg)

	if *validateOnly {
		logger.Info("配置校验通过")
		return nil
	}

	return serve(cfg, logger, level)
}

// loadConfig 加载配置并应用命令行覆盖。flag 优先级最高。
//
// 路径归一化交给 config.Apply 统一处理——main 里自己再写一份斜杠剥离逻辑，
// 早晚会和 normalize() 走偏。
func loadConfig(path string, overrides config.Overrides) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Apply(overrides); err != nil {
		return nil, oerr.Wrap(err, "命令行参数校验失败")
	}
	return cfg, nil
}

// serve 装配采集链路与 HTTP 服务，并交给 bootstrap 托管生命周期。
func serve(cfg *config.Config, logger logx.Logger, level logx.Level) error {
	specs, err := buildSpecs(cfg)
	if err != nil {
		return err
	}

	set, err := collector.NewSet(specs, collector.SetOptions{
		Namespace: cfg.Namespace,
		Version:   version,
		Logger:    logx.Delegating("collector"),
	})
	if err != nil {
		return oerr.Wrap(err, "初始化采集器失败")
	}
	for _, w := range set.SchemaWarnings() {
		logger.Warn("各台采集的接口集合不一致", "detail", w)
	}
	for _, inst := range set.Instances() {
		logger.Info("路由器已接入",
			"host", inst.Spec.Host, "ip", inst.Spec.IP,
			"profile", inst.Client.Profile().Name(),
			"endpoints", inst.Client.Profile().DescribeEndpoints(),
			"mode", inst.Source.Mode())
	}

	registry := prometheus.NewRegistry()
	// 只注册业务指标：exporter 自身的 Go runtime / process 指标会引入
	// go_* / process_* 一大票与路由器无关的序列，污染小型 Prometheus 的存储。
	if err := set.Register(registry); err != nil {
		return oerr.Wrap(err, "注册指标失败")
	}

	h, err := server.New(server.Options{
		Config:           cfg,
		Registry:         registry,
		ReadyCheck:       set.Ready,
		ContextCollector: set,
		RouterStatus:     func() any { return set.Status() },
		Logger:           logx.Delegating("server"),
		LogLevel:         level,
	})
	if err != nil {
		return oerr.Wrap(err, "装配 HTTP 服务失败")
	}

	b, err := bootstrap.New(
		bootstrap.WithServiceName(server.ServiceName),
		bootstrap.WithShutdownTimeout(cfg.ShutdownTimeout),
	)
	if err != nil {
		return oerr.Wrap(err, "初始化 bootstrap 失败")
	}

	return b.Run(func(app *bootstrap.App) error {
		app.AddResource("routers", set.Close)
		set.StartBackground(app)

		ekithertz.RunServer(app, "http", h)
		app.Ready()

		logger.Info("miwifi-exporter 已启动",
			"routers", cfg.RouterStrings(),
			"metrics", fmt.Sprintf("http://localhost:%d%s", cfg.Port, cfg.MetricsPath))

		// 硬规则：userFn 返回即服务结束，长驻服务必须阻塞在这里。
		<-app.Context().Done()
		logger.Info("miwifi-exporter 收到关停信号")
		return nil
	})
}

// buildSpecs 把配置翻译成采集层的实例规格。
func buildSpecs(cfg *config.Config) ([]collector.RouterSpec, error) {
	globalTimeouts, err := cfg.ParsedEndpointTimeouts()
	if err != nil {
		return nil, err
	}

	specs := make([]collector.RouterSpec, 0, len(cfg.Routers))
	for _, r := range cfg.Routers {
		perRouterTimeouts, err := r.ParsedEndpointTimeouts()
		if err != nil {
			return nil, err
		}
		// 按台覆盖与全局覆盖合并，按台优先。
		merged := make(map[string]time.Duration, len(globalTimeouts)+len(perRouterTimeouts))
		for k, v := range globalTimeouts {
			merged[k] = v
		}
		for k, v := range perRouterTimeouts {
			merged[k] = v
		}

		mode := r.EffectiveCollectMode(cfg.CollectMode)
		interval := r.EffectiveInterval(cfg.CollectInterval)
		if mode == collector.ModePoll && interval <= 0 {
			interval = cfg.CollectInterval
		}

		specs = append(specs, collector.RouterSpec{
			Host:             r.Host,
			IP:               r.IP,
			BaseURL:          r.BaseURL(),
			Password:         r.Password,
			Profile:          r.EffectiveProfile(cfg.RouterProfile),
			Timeout:          r.EffectiveTimeout(cfg.RouterTimeout),
			Mode:             mode,
			Interval:         interval,
			ExcludeEndpoints: r.ExcludeEndpoints,
			EndpointTimeouts: merged,
			TimeoutThreshold: cfg.EndpointTimeoutThreshold,
		})
	}
	return specs, nil
}
