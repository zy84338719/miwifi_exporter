package router_test

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"

	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// newRetryClient 构造一个带完整重试参数的客户端。
//
// 不用 newClient：那个把 Attempts 设成 1，恰好绕开了「重试放大」这条路径。
func newRetryClient(t *testing.T, fake *testutil.FakeRouter, opts router.Options) *router.Client {
	t.Helper()
	opts.BaseURL = fake.URL()
	if opts.Password == "" {
		opts.Password = "secret"
	}
	if opts.Retry.Attempts == 0 {
		opts.Retry = retry.Config{Attempts: 3, Delay: time.Millisecond}
	}
	c, err := router.New(opts)
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// watchedEndpoints 是「每轮都会采到」的端点，用来看请求次数。
var watchedEndpoints = []string{
	"api/misystem/status",
	"api/xqnetwork/lan_info",
	"api/misystem/devicelist",
}

// TestStokInvalidEnvelopeTriggersRelogin 覆盖真机的 stok 失效形态。
//
// 真机回的是 HTTP 200 + {"code":401,"msg":"Invalid token"}，不是 401/403。
// 过去 exporter 只看 HTTP 状态码：这个响应被当成成功采集，载荷解析成全零结构，
// 端点记成成功，于是 mac/sn 变空串、uptime 变 0、设备级序列整片消失，
// 而且永远不会重新登录——「指标大面积消失但一切看起来正常」。
func TestStokInvalidEnvelopeTriggersRelogin(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newRetryClient(t, fake, router.Options{Timeout: 3 * time.Second, StokTTL: time.Hour})

	if snap := c.FetchAll(context.Background()); !snap.Healthy() {
		t.Fatalf("首轮应全部成功: %v", snap.FailedEndpoints())
	}
	loginsBefore := fake.LoginCalls()

	// 路由器重启：stok 失效（真机形态 = 200 + code:401）。
	fake.RotateStok()

	callsBefore := map[string]int{}
	for _, ep := range watchedEndpoints {
		callsBefore[ep] = fake.Calls(ep)
	}

	c.FetchAll(context.Background())

	if fake.LoginCalls() <= loginsBefore {
		t.Errorf("stok 失效后应触发重新登录，登录次数 %d → %d", loginsBefore, fake.LoginCalls())
	}

	// 失效的那一轮里，每个端点最多被请求 2 次：1 次带失效 stok + 1 次带新 stok。
	// 若 stok 失效被通用重试放大，这里会是 4 次（3 次重试 + 1 次自愈）。
	for _, ep := range watchedEndpoints {
		if delta := fake.Calls(ep) - callsBefore[ep]; delta > 2 {
			t.Errorf("%s 在失效那一轮被请求 %d 次：stok 失效不该被通用重试放大（期望 ≤2）", ep, delta)
		}
	}

	// 下一轮必须完全自愈：指标不能继续是空结构。
	if snap := c.FetchAll(context.Background()); !snap.Healthy() {
		t.Fatalf("stok 失效后应自动重登并恢复: %v", snap.FailedEndpoints())
	}
	if snap := c.FetchAll(context.Background()); snap.Status.CPU.Core != 4 {
		t.Errorf("自愈后数据不正确: %+v", snap.Status.CPU)
	}
}

// TestStokFailure403StillHeals 覆盖「回 403 的固件」这条路径。
//
// 部分固件确实回 403，那条自愈路径必须照常工作。
func TestStokFailure403StillHeals(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	fake.SetStokFailureStatus(403)
	c := newRetryClient(t, fake, router.Options{Timeout: 3 * time.Second, StokTTL: time.Hour})

	if snap := c.FetchAll(context.Background()); !snap.Healthy() {
		t.Fatalf("首轮应全部成功: %v", snap.FailedEndpoints())
	}
	fake.RotateStok()
	c.FetchAll(context.Background())

	if snap := c.FetchAll(context.Background()); !snap.Healthy() {
		t.Fatalf("403 形态的 stok 失效也应自愈: %v", snap.FailedEndpoints())
	}
}

// TestMissingEndpointIsNotRetried 验证 404 只问一次。
//
// 「该固件没有这个接口」是一次就能确定的事实，重试 3 次只是把同一个 404
// 又问了两遍，还白等 200/400ms 退避——而且与代码里「404 一次就确定」的
// 处置相矛盾。
func TestMissingEndpointIsNotRetried(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	fake.SetMissing("api/misystem/status", true)
	c := newRetryClient(t, fake, router.Options{Timeout: 3 * time.Second})

	snap := c.FetchAll(context.Background())

	if st := snap.Endpoint["status"]; st.OK {
		t.Fatal("404 的端点不应被报告为成功")
	}
	if n := fake.Calls("api/misystem/status"); n != 1 {
		t.Errorf("404 端点被请求 %d 次，期望 1 次（确定不存在的事实不该重试）", n)
	}
}

// TestErrorEnvelopeIsNotTreatedAsSuccess 验证错误信封不会被当成一次成功采集。
func TestErrorEnvelopeIsNotTreatedAsSuccess(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	fake.SetPayload("api/misystem/status", `{"code":401,"msg":"Invalid token"}`)
	c := newRetryClient(t, fake, router.Options{Timeout: 3 * time.Second})

	snap := c.FetchAll(context.Background())

	st := snap.Endpoint["status"]
	if st.OK {
		t.Fatal("错误信封不应被记成一次成功采集")
	}
	if st.Err == nil {
		t.Fatal("错误信封应带上失败原因")
	}
}

// TestEndpointTimeoutNotCappedByClientTimeout 验证端点级超时预算真的生效。
//
// 过去 http.Client.Timeout（= 全局预算）会把按端点分配的预算一并压掉，
// 于是 profile 内置的 wifi_list=20s、用户覆盖的 lan_info=8s 全部等于没写。
func TestEndpointTimeoutNotCappedByClientTimeout(t *testing.T) {
	fake := testutil.NewFakeRouterWithDelay(t, 2*time.Second)
	c := newRetryClient(t, fake, router.Options{
		Timeout: 1 * time.Second,
		EndpointTimeouts: map[string]time.Duration{
			"status": 5 * time.Second,
		},
	})

	snap := c.FetchAll(context.Background())

	if st := snap.Endpoint["status"]; !st.OK {
		t.Errorf("端点级超时预算 5s 应覆盖全局 1s，实际失败: %v", st.Err)
	}
}
