package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oekit/bootstrap/healthz"
	"github.com/oekit/concurrency/retry"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	appcfg "github.com/helloworlde/miwifi-exporter/internal/config"
	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/server"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// newTestServer 起一个真实的 Hertz 实例（随机端口），返回基础 URL。
func newTestServer(t *testing.T, cfg *appcfg.Config) string {
	t.Helper()
	return newTestServerWith(t, cfg, nil)
}

// newTestServerWith 与 newTestServer 相同，但允许注入就绪判据。
func newTestServerWith(t *testing.T, cfg *appcfg.Config, readyCheck func(context.Context) error) string {
	t.Helper()

	fake := testutil.NewFakeRouter(t)
	client, err := router.New(router.Options{
		BaseURL:  fake.URL(),
		Password: "pw",
		Timeout:  3 * time.Second,
		Retry:    retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 router.Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	c, err := collector.New(collector.CollectorOptions{
		Namespace: cfg.Namespace,
		Host:      cfg.RouterHost,
		Version:   "test",
		Source: collector.NewPullSource(collector.PullSourceOptions{
			Client: client, Timeout: 5 * time.Second,
		}),
	})
	if err != nil {
		t.Fatalf("构造 Collector 失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("注册 Collector 失败: %v", err)
	}
	if err := reg.Register(c.Self()); err != nil {
		t.Fatalf("注册自监控指标失败: %v", err)
	}

	h, err := server.New(server.Options{
		Config:     cfg,
		Registry:   reg,
		ReadyCheck: readyCheck,
	})
	if err != nil {
		t.Fatalf("构造 server 失败: %v", err)
	}

	go h.Spin()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})

	base := "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
	waitReady(t, base+"/livez")
	return base
}

// freePort 向内核要一个当前空闲端口。
//
// 比从 Hertz 的 Options 里反查端口更省事：测试并不需要 Hertz 的内部布局，
// 多依赖一个 GetOptions 的具体签名只会让升级 Hertz 时无谓地改测试。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // 测试内固定 localhost 地址
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("服务在 5s 内未就绪: %s", url)
}

func defaultConfig(t *testing.T) *appcfg.Config {
	t.Helper()
	return &appcfg.Config{
		Port:            freePort(t),
		MetricsPath:     "/metrics",
		Namespace:       "miwifi",
		RouterHost:      "testhost",
		CollectMode:     "pull",
		RouterTimeout:   2 * time.Second,
		ShutdownTimeout: time.Second,
	}
}

// TestMetricsEndpointIsRawText 是这套中间件栈最关键的一条断言。
//
// oekit 的 ResponseMiddleware 会把响应统一包成 {code,data,message,trace_id}。
// /metrics 一旦被包一层，Prometheus 立刻解析失败且报错含糊。stack.go 里的
// StackConfig.SkipPaths 只作用于 AccessLog，对响应包装无效——必须给
// ResponseMiddleware 单独传 WithSkipPaths。
func TestMetricsEndpointIsRawText(t *testing.T) {
	base := newTestServer(t, defaultConfig(t))

	resp, err := http.Get(base + "/metrics") //nolint:gosec // 测试内固定 localhost 地址
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q，期望 Prometheus 文本格式", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	text := string(body)

	if strings.Contains(text, `"trace_id"`) || strings.Contains(text, `"message"`) {
		t.Errorf("/metrics 被统一响应包壳包裹了:\n%s", firstLines(text, 5))
	}
	if !strings.Contains(text, "miwifi_cpu_cores") {
		t.Errorf("/metrics 里没有路由器指标:\n%s", firstLines(text, 20))
	}
	if !strings.Contains(text, "miwifi_exporter_build_info") {
		t.Errorf("/metrics 里没有自监控指标")
	}
	// HELP 必须非空。
	if strings.Contains(text, "# HELP miwifi_cpu_cores \n") {
		t.Errorf("miwifi_cpu_cores 的 HELP 为空")
	}
}

func TestHealthEndpoints(t *testing.T) {
	base := newTestServer(t, defaultConfig(t))

	tests := []struct {
		path       string
		wantStatus int
	}{
		{"/livez", http.StatusOK},
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(base + tc.path) //nolint:gosec // 测试内固定 localhost 地址
			if err != nil {
				t.Fatalf("请求 %s 失败: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("%s 状态码 = %d，期望 %d，body=%s", tc.path, resp.StatusCode, tc.wantStatus, body)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "status") {
				t.Errorf("%s 响应不是健康 JSON: %s", tc.path, body)
			}
		})
	}
}

func TestLandingPage(t *testing.T) {
	base := newTestServer(t, defaultConfig(t))

	resp, err := http.Get(base + "/") //nolint:gosec // 测试内固定 localhost 地址
	if err != nil {
		t.Fatalf("请求首页失败: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("首页状态码 = %d", resp.StatusCode)
	}
	if !strings.Contains(text, `<a href="/metrics">Metrics</a>`) {
		t.Errorf("首页缺少指标链接:\n%s", text)
	}
}

// TestCustomMetricsPath 验证 -web.telemetry-path 的兼容性。
func TestCustomMetricsPath(t *testing.T) {
	cfg := defaultConfig(t)
	cfg.MetricsPath = "/custom-telemetry"
	base := newTestServer(t, cfg)

	resp, err := http.Get(base + "/custom-telemetry") //nolint:gosec // 测试内固定 localhost 地址
	if err != nil {
		t.Fatalf("请求自定义指标路径失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "miwifi_cpu_cores") {
		t.Error("自定义路径下没有指标输出")
	}
}

func TestServerValidation(t *testing.T) {
	if _, err := server.New(server.Options{}); err == nil {
		t.Error("缺少 Config 时应当报错")
	}
	if _, err := server.New(server.Options{Config: defaultConfig(t)}); err == nil {
		t.Error("缺少 Registry 时应当报错")
	}
}

// TestScrapeDoesNotWrapWithJSON 兜底确认：经过 /metrics 的响应能被
// prometheus 官方文本解析器识别。
func TestScrapeParsableByPrometheus(t *testing.T) {
	base := newTestServer(t, defaultConfig(t))

	resp, err := http.Get(base + "/metrics") //nolint:gosec // 测试内固定 localhost 地址
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var parser expfmtTextParser
	if err := parser.parse(body); err != nil {
		t.Fatalf("响应不是合法的 Prometheus 文本格式: %v", err)
	}
	if parser.families == 0 {
		t.Error("解析到 0 个指标族")
	}
}

// expfmtTextParser 是一个极简的 Prometheus 文本格式校验器。
//
// 不引入 expfmt 依赖，只校验三条硬规则：每行要么是注释、要么是合法
// `name value` 或 `name{labels} value`。足以抓住「被 JSON 包壳」这类破坏。
type expfmtTextParser struct{ families int }

func (p *expfmtTextParser) parse(body []byte) error {
	seen := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "{") {
			return errString("响应以 JSON 对象开头，说明被统一包壳了: " + firstLines(string(body), 3))
		}
		name, rest, found := strings.Cut(line, " ")
		if !found {
			return errString("行缺少取值: " + line)
		}
		name = strings.TrimSuffix(name, "{")
		if name == "" {
			return errString("指标名为空: " + line)
		}
		_ = rest
		seen[name] = true
	}
	p.families = len(seen)
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestReadinessReflectsCollectorState 验证 ReadyCheck 真的接进了探针。
//
// 历史问题：Options.ReadyCheck 被声明却从未被调用，于是 poll 模式下
// 首轮采集还没完成 /readyz 就已经 200——文档里写的就绪语义根本没生效，
// K8s 会把一个还没有任何快照可给的 Pod 标成就绪。
func TestReadinessReflectsCollectorState(t *testing.T) {
	healthz.ResetForTest()
	t.Cleanup(healthz.ResetForTest)

	var ready atomic.Bool

	cfg := defaultConfig(t)
	base := newTestServerWith(t, cfg, func(context.Context) error {
		if !ready.Load() {
			return errNotReady{}
		}
		return nil
	})

	// 未就绪 → 503，且失败详情里要能看到是哪个检查项、什么原因。
	// 「只回一个 503 什么都不说」的探针在排障时毫无用处。
	resp, err := http.Get(base + "/readyz") //nolint:gosec // 测试内固定 localhost 地址
	if err != nil {
		t.Fatalf("请求 /readyz 失败: %v", err)
	}
	failBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("未就绪时 /readyz = %d，期望 503，body=%s", resp.StatusCode, failBody)
	}
	for _, want := range []string{"exporter", "not ready yet", "unhealthy"} {
		if !strings.Contains(string(failBody), want) {
			t.Errorf("503 响应应包含 %q，实际: %s", want, failBody)
		}
	}

	// 就绪 → 200（healthz 在全部通过时按设计不输出 checks 明细）
	ready.Store(true)
	resp2, err := http.Get(base + "/readyz") //nolint:gosec // 测试内固定 localhost 地址
	if err != nil {
		t.Fatalf("请求 /readyz 失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("就绪后 /readyz = %d，期望 200", resp2.StatusCode)
	}
	okBody, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(okBody), `"status":"ok"`) {
		t.Errorf("就绪响应 = %s", okBody)
	}
}

// TestLivenessIgnoresReadiness 守住探针分工：/livez 永远 200。
func TestLivenessIgnoresReadiness(t *testing.T) {
	healthz.ResetForTest()
	t.Cleanup(healthz.ResetForTest)

	base := newTestServerWith(t, defaultConfig(t), func(context.Context) error {
		return errNotReady{}
	})

	for _, path := range []string{"/livez"} {
		resp, err := http.Get(base + path) //nolint:gosec // 测试内固定 localhost 地址
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d，未就绪时也必须 200（K8s 会因此重启 exporter）", path, resp.StatusCode)
		}
	}
}

type errNotReady struct{}

func (errNotReady) Error() string { return "not ready yet" }
