package router_test

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

func newProfiledClient(t *testing.T, fake *testutil.FakeRouter, profileName string) *router.Client {
	t.Helper()
	c, err := router.New(router.Options{
		BaseURL:     fake.URL(),
		Password:    "pw",
		Timeout:     3 * time.Second,
		ProfileName: profileName,
		Retry:       retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// TestMinimalProfileSkipsUnsupportedEndpoints 验证 profile 能在请求发出之前
// 就跳过不支持的端点——而不是发出去再失败再打 WARN。
func TestMinimalProfileSkipsUnsupportedEndpoints(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newProfiledClient(t, fake, profile.Minimal)

	snap := c.FetchAll(context.Background())

	if !snap.Healthy() {
		t.Fatalf("minimal profile 下应全部成功: %v", snap.FailedEndpoints())
	}
	if got := len(snap.Endpoint); got != 2 {
		t.Errorf("minimal profile 只应采 2 个端点，实际 %d: %v", got, snap.FailedEndpoints())
	}
	for _, ep := range []string{profile.EPWanInfo, profile.EPLanInfo, profile.EPWifiList} {
		if _, asked := snap.Endpoint[ep]; asked {
			t.Errorf("minimal profile 不应采集端点 %s", ep)
		}
		if n := fake.Calls(apiPathOf(ep)); n != 0 {
			t.Errorf("端点 %s 被请求了 %d 次，minimal profile 下应为 0 次", ep, n)
		}
	}
}

// TestAdaptiveDegradation 验证自适应探测：固件明确说「没这个接口」后，
// 后续轮次就不再打扰它了。
//
// 这正是老机型上旧版 exporter 的痛点：每轮 scrape 都为「wifi_list 根本不存在」
// 打一条 WARN，而指标恒为 0 却没人分得清是没这接口还是它坏了。
func TestAdaptiveDegradation(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newProfiledClient(t, fake, profile.Default)

	// 让 wifi_list 表现得像「这台机器上没有这个接口」。
	fake.SetFail("api/xqnetwork/wifi_list", true)

	first := c.FetchAll(context.Background())
	if first.Endpoint["wifi_list"].OK {
		t.Fatal("首轮应报告 wifi_list 失败")
	}

	// 模拟路由器对不存在的接口返回 404 而非 500。
	fake.SetFail("api/xqnetwork/wifi_list", false)
	fake.SetMissing("api/xqnetwork/wifi_list", true)

	second := c.FetchAll(context.Background())

	if !clientHasDisabled(c, "wifi_list") {
		t.Fatalf("收到 404 后应把 wifi_list 标记为不支持，实际 disabled=%v",
			c.Profile().DisabledEndpoints())
	}
	if second.Endpoint["wifi_list"].OK {
		t.Error("收到 404 的那一轮本身仍应报告失败")
	}

	callsAtDisable := fake.Calls("api/xqnetwork/wifi_list")

	// 再采三轮：wifi_list 不该再被请求。
	for i := 0; i < 3; i++ {
		snap := c.FetchAll(context.Background())
		if _, ok := snap.Endpoint["wifi_list"]; ok {
			t.Errorf("第 %d 轮后 wifi_list 已不应出现在本轮快照里", i+2)
		}
	}
	if got := fake.Calls("api/xqnetwork/wifi_list"); got != callsAtDisable {
		t.Errorf("被判定不支持后仍在请求：调用次数 %d → %d", callsAtDisable, got)
	}
}

// TestRecheckRevivesEndpoint 验证被禁用的端点会被定期放回探测名单，
// 避免「自适应降级」变成「自适应遗忘」。
func TestRecheckRevivesEndpoint(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newProfiledClient(t, fake, profile.Default)

	fake.SetFail("api/xqnetwork/lan_info", true)
	fake.SetMissing("api/xqnetwork/lan_info", true)
	c.FetchAll(context.Background())

	if !clientHasDisabled(c, "lan_info") {
		t.Fatal("lan_info 应被判定为不支持")
	}

	// 模拟固件升级：接口回来了。
	fake.SetMissing("api/xqnetwork/lan_info", false)
	fake.SetFail("api/xqnetwork/lan_info", false)

	// 复探间隔未到，不会立刻恢复——这正是我们要的，避免每轮都试。
	before := fake.Calls("api/xqnetwork/lan_info")
	c.FetchAll(context.Background())
	if got := fake.Calls("api/xqnetwork/lan_info"); got != before {
		t.Errorf("复探间隔未到却发起了请求：%d → %d", before, got)
	}
}

// TestUnknownProfileRejected 配置里写错 profile 名要立刻失败，而不是静默回落。
func TestUnknownProfileRejected(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	_, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw", ProfileName: "no-such-profile",
	})
	if err == nil {
		t.Fatal("未知 profile 应当报错")
	}
}

func TestDefaultProfileRequestsEverything(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newProfiledClient(t, fake, profile.Auto)

	c.FetchAll(context.Background())

	for _, ep := range profile.AllEndpoints {
		if fake.Calls(apiPathOf(ep)) == 0 {
			t.Errorf("auto/default profile 应请求端点 %s，实际没请求", ep)
		}
	}
}

func apiPathOf(endpoint string) string {
	if isXQ(endpoint) {
		return "api/xqnetwork/" + endpoint
	}
	return "api/misystem/" + endpoint
}

func isXQ(endpoint string) bool {
	switch endpoint {
	case profile.EPWanInfo, profile.EPWifiDetailAll, profile.EPPppoeStatus,
		profile.EPLanInfo, profile.EPWifiConnDevices, profile.EPWifiList:
		return true
	}
	return false
}

func clientHasDisabled(c *router.Client, endpoint string) bool {
	for _, ep := range c.Profile().DisabledEndpoints() {
		if ep == endpoint {
			return true
		}
	}
	return false
}

// TestExcludeEndpointsUnknownRejected 配置里写错端点名要立刻失败。
//
// 静默忽略会让用户以为端点被排除了，实际上还在采——排查起来极其费时。
func TestExcludeEndpointsUnknownRejected(t *testing.T) {
	fake := testutil.NewFakeRouter(t)

	if _, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw",
		ExcludeEndpoints: []string{"no_such_endpoint"},
	}); err == nil {
		t.Error("排除未知端点应当报错")
	}

	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw",
		ExcludeEndpoints: []string{"wifi_list"},
	})
	if err != nil {
		t.Fatalf("排除合法端点失败: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	// 被排除的端点连请求都不该发出。
	snap := c.FetchAll(context.Background())
	if _, ok := snap.Endpoint["wifi_list"]; ok {
		t.Error("被排除的端点不应出现在采集结果里")
	}
	if n := fake.Calls("api/xqnetwork/wifi_list"); n != 0 {
		t.Errorf("被排除的端点仍被请求了 %d 次", n)
	}
	if n := fake.Calls("api/misystem/status"); n == 0 {
		t.Error("其余端点应照常采集")
	}
}

// TestEndpointTimeoutOverride 验证按端点放宽超时会真正下发到请求层。
func TestEndpointTimeoutOverride(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw",
		Timeout:          2 * time.Second,
		EndpointTimeouts: map[string]time.Duration{"wifi_list": 9 * time.Second},
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if got := c.EndpointTimeoutOf("api/xqnetwork/wifi_list"); got != 9*time.Second {
		t.Errorf("wifi_list 超时 = %v，期望 9s", got)
	}
	// 未覆盖的端点沿用全局值。
	if got := c.EndpointTimeoutOf("api/misystem/status"); got != 2*time.Second {
		t.Errorf("status 超时 = %v，期望沿用全局 2s", got)
	}
	// 未知路径退回全局值，不应 panic 也不应为 0。
	if got := c.EndpointTimeoutOf("api/unknown/thing"); got != 2*time.Second {
		t.Errorf("未知路径超时 = %v，期望 2s", got)
	}
}

// TestRepeatedNonMissingFailureDegrades 端到端验证「重复失败降级」。
//
// 真实案例：一台 R3P 上 pppoe_status 反复失败但不是 404，自适应探测因此
// 永远不收敛。结果是 scrape_total{result="partial"} 只增不减，一次干净的
// success 都不会出现，而 endpoint_supported 始终说 1。
//
// 这里用假路由器的 500（而不是 404）复现：500 不触发即时的 ErrEndpointMissing
// 路径，只能靠连续重复降级收敛。
func TestRepeatedNonMissingFailureDegrades(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newProfiledClient(t, fake, profile.Default)

	fake.SetFail("api/xqnetwork/pppoe_status", true)

	for i := 1; i <= profile.RepeatedFailureThreshold; i++ {
		snap := c.FetchAll(context.Background())
		if snap.Endpoint["pppoe_status"].OK {
			t.Fatalf("第 %d 轮 pppoe_status 不该成功", i)
		}
		if i < profile.RepeatedFailureThreshold && clientHasDisabled(c, "pppoe_status") {
			t.Fatalf("第 %d 轮就停问了，阈值 %d 形同虚设", i, profile.RepeatedFailureThreshold)
		}
	}

	if !clientHasDisabled(c, "pppoe_status") {
		t.Fatalf("连续 %d 轮相同错误后应停问 pppoe_status，实际 disabled=%v",
			profile.RepeatedFailureThreshold, c.Profile().DisabledEndpoints())
	}

	// 停问之后不再发请求——这正是真机上 scrape 耗时从 3.8s 降下来的原因。
	callsAtDisable := fake.Calls("api/xqnetwork/pppoe_status")
	snap := c.FetchAll(context.Background())
	if _, ok := snap.Endpoint["pppoe_status"]; ok {
		t.Error("停问后 pppoe_status 不应再出现在本轮快照里")
	}
	if got := fake.Calls("api/xqnetwork/pppoe_status"); got != callsAtDisable {
		t.Errorf("停问后仍在请求：%d → %d", callsAtDisable, got)
	}
}

// TestTimeoutDoesNotTriggerRepeatedDegradation 守住最重要的一条安全阀。
//
// 超时分不清「这个端点坏了」和「路由器被别的慢端点堵住了」。真机实测里
// wifi_list 触发 WiFi 扫描会阻塞其余全部 LuCI 请求，连带九个健康端点超时。
// 所以超时的错误文本里带着每次都不同的耗时，天然不会累积成停问理由。
func TestTimeoutDoesNotTriggerRepeatedDegradation(t *testing.T) {
	fake := testutil.NewFakeRouterWithDelay(t, 2*time.Second)
	c := newProfiledClient(t, fake, profile.Default)

	// 超时预算 50ms，远小于上面的 2s 延迟。
	c2, err := router.New(router.Options{
		BaseURL:     fake.URL(),
		Password:    "pw",
		Timeout:     50 * time.Millisecond,
		ProfileName: profile.Default,
		Retry:       retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close(context.Background()) })
	_ = c

	for i := 1; i <= profile.RepeatedFailureThreshold+2; i++ {
		c2.FetchAll(context.Background())
	}

	if clientHasDisabled(c2, profile.EPStatus) {
		t.Fatalf("端点只是慢，不该因此被停问（disabled=%v）",
			c2.Profile().DisabledEndpoints())
	}
}

// TestLastErrorsExposesFailureReason 验证失败原因能被 /routers 读到。
//
// 指标只能告诉你「哪个端点挂了」，说不出「为什么挂」。这次排查一台 R3P
// 时，pppoe_status 反复失败而原因只出现在容器日志里——查的人却未必有
// 那台机器的 ssh。
func TestLastErrorsExposesFailureReason(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newProfiledClient(t, fake, profile.Default)

	if errs := c.LastErrors(); len(errs) != 0 {
		t.Fatalf("采集前不应有任何错误记录，实际 %v", errs)
	}

	fake.SetFail("api/xqnetwork/pppoe_status", true)
	c.FetchAll(context.Background())

	errs := c.LastErrors()
	text, ok := errs["pppoe_status"]
	if !ok {
		t.Fatalf("LastErrors 应包含 pppoe_status，实际 %v", errs)
	}
	if text == "" {
		t.Error("错误文本不应为空")
	}
	for ep := range errs {
		if ep == "pppoe_status" {
			continue
		}
		t.Errorf("成功的端点 %s 不应出现在 LastErrors 里", ep)
	}

	// 恢复之后错误记录必须清空，否则 /routers 会一直显示早已修好的故障。
	fake.SetFail("api/xqnetwork/pppoe_status", false)
	c.FetchAll(context.Background())
	if errs := c.LastErrors(); len(errs) != 0 {
		t.Errorf("恢复后 LastErrors 应为空，实际 %v", errs)
	}
}

// TestExcludeEndpointsSurvivesRecheck 端到端守住「配置排除永久生效」。
//
// 真机事故：一台 R3P 的 compose 里写了 excludeEndpoints:["pppoe_status"]，
// 启动日志也确实打了「按配置排除端点」，但 30 分钟后的复探把它放回了探测
// 名单，此后每轮 scrape 都 partial。配置看着一直生效，实际早已失效。
func TestExcludeEndpointsSurvivesRecheck(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c, err := router.New(router.Options{
		BaseURL:          fake.URL(),
		Password:         "pw",
		Timeout:          3 * time.Second,
		ProfileName:      profile.NoScan,
		ExcludeEndpoints: []string{profile.EPPppoeStatus},
		Retry:            retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	// 第一轮：端点压根不进快照。
	snap := c.FetchAll(context.Background())
	if _, ok := snap.Endpoint["pppoe_status"]; ok {
		t.Fatal("配置排除后 pppoe_status 不应出现在本轮快照里")
	}
	callsAtExclude := fake.Calls("api/xqnetwork/pppoe_status")

	// 复探间隔是 30 分钟，等不起。RecheckDue 内部调的就是 RetryDisabled，
	// 这里直接给它 1ms 的 ttl 来模拟「时间到了」。
	time.Sleep(2 * time.Millisecond)
	c.Profile().RetryDisabled(1)

	snap = c.FetchAll(context.Background())
	if _, ok := snap.Endpoint["pppoe_status"]; ok {
		t.Fatal("复探把配置排除的 pppoe_status 放回了——配置排除必须永久生效")
	}
	if got := fake.Calls("api/xqnetwork/pppoe_status"); got != callsAtExclude {
		t.Errorf("配置排除后仍在请求：%d → %d", callsAtExclude, got)
	}
}
