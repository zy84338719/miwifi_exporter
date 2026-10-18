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
