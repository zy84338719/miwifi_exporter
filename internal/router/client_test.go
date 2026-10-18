package router_test

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"

	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

func newClient(t *testing.T, fake *testutil.FakeRouter) *router.Client {
	t.Helper()
	c, err := router.New(router.Options{
		BaseURL:  fake.URL(),
		Password: "secret",
		Timeout:  3 * time.Second,
		StokTTL:  time.Hour,
		Retry:    retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestFetchAllHappyPath(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newClient(t, fake)

	snap := c.FetchAll(context.Background())

	if !snap.Healthy() {
		t.Fatalf("期望全部端点成功，实际失败: %v", snap.FailedEndpoints())
	}
	if len(snap.Endpoint) != 10 {
		t.Errorf("期望 10 个端点，实际 %d 个", len(snap.Endpoint))
	}
	if snap.Status.CPU.Core != 4 {
		t.Errorf("CPU 核心数 = %d，期望 4", snap.Status.CPU.Core)
	}
	if len(snap.WanInfo.Ipv4) != 1 || snap.WanInfo.Ipv4[0].IP != "192.168.31.200" {
		t.Errorf("WAN IPv4 解析异常: %+v", snap.WanInfo.Ipv4)
	}
	if len(snap.LanInfo.LinkList) != 4 {
		t.Errorf("LAN 链路数 = %d，期望 4", len(snap.LanInfo.LinkList))
	}
	if fake.LoginCalls() != 1 {
		t.Errorf("登录次数 = %d，期望 1（一次采集只登录一次）", fake.LoginCalls())
	}
}

// TestStokExpiryTriggersRelogin 覆盖路由器重启导致 stok 失效的主路径。
//
// 这是旧版最脆弱的地方：stok 只在进程启动时取一次，路由器一重启，
// exporter 就永久返回垃圾数据，而且没有任何指标能说明发生了什么。
func TestStokExpiryTriggersRelogin(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newClient(t, fake)

	if snap := c.FetchAll(context.Background()); !snap.Healthy() {
		t.Fatalf("首次采集应当成功: %v", snap.FailedEndpoints())
	}
	loginsAfterFirst := fake.LoginCalls()

	// 路由器重启：stok 失效。fetch 是单次尝试，失效后下一轮会重新登录。
	fake.SetStok("new-stok-after-reboot")
	c.ResetLastGood()

	snap := c.FetchAll(context.Background())
	if !snap.Healthy() {
		t.Fatalf("stok 失效后应自动重登并成功: %v", snap.FailedEndpoints())
	}
	if fake.LoginCalls() <= loginsAfterFirst {
		t.Errorf("stok 失效后应触发重新登录，登录次数 %d → %d", loginsAfterFirst, fake.LoginCalls())
	}
	if snap.Status.CPU.Core != 4 {
		t.Errorf("重登后数据不正确: %+v", snap.Status.CPU)
	}
}

// TestPartialFailureKeepsLastGood 验证「端点失败时回退到上次成功值」。
//
// 期望行为：lan_info 挂了，lan 相关的指标继续导出上一轮的值，并被标记为
// stale。这样 Prometheus 侧的 rate() 不会因为一个端点抖动就整段断掉。
func TestPartialFailureKeepsLastGood(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newClient(t, fake)

	first := c.FetchAll(context.Background())
	if !first.Healthy() {
		t.Fatalf("首轮应全部成功: %v", first.FailedEndpoints())
	}
	wantUptime := first.LanInfo.Info.Uptime

	fake.SetFail("api/xqnetwork/lan_info", true)
	second := c.FetchAll(context.Background())

	st, ok := second.Endpoint["lan_info"]
	if !ok {
		t.Fatal("快照里缺少 lan_info 的状态")
	}
	if st.OK {
		t.Fatal("lan_info 被置为持续失败，本轮却报告成功")
	}
	if !st.Stale {
		t.Error("lan_info 应当被标记为使用了陈旧值")
	}
	if second.LanInfo.Info.Uptime != wantUptime {
		t.Errorf("lan_info 应当保留上次成功的 uptime=%d，实际 %d",
			wantUptime, second.LanInfo.Info.Uptime)
	}
	// 其它端点不受影响。
	if !second.Endpoint["status"].OK {
		t.Error("单个端点失败不应波及其它端点")
	}
	if got := second.FailedEndpoints(); len(got) != 1 || got[0] != "lan_info" {
		t.Errorf("FailedEndpoints = %v，期望 [lan_info]", got)
	}
}

// TestNoLastGoodOnFirstFailure 验证首轮就失败时不会伪造数据，
// 此时 Stale 必须为 false。
func TestNoLastGoodOnFirstFailure(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	fake.SetFail("api/xqnetwork/lan_info", true)
	c := newClient(t, fake)

	snap := c.FetchAll(context.Background())

	st := snap.Endpoint["lan_info"]
	if st.OK {
		t.Fatal("应当失败")
	}
	if st.Stale {
		t.Error("首轮失败没有历史值可用，不应标记为 stale")
	}
	if snap.LanInfo.Info.Uptime != 0 {
		t.Errorf("无留档时应为 0，实际 %d", snap.LanInfo.Info.Uptime)
	}
}

// TestRetrySucceedsAfterTransientFailure 验证单次失败的重试能自愈。
func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	// 第一次调用 500，第二次正常——真实路由器在忙时就是这样。
	fake.FailTimes("api/misystem/status", 1)

	c, err := router.New(router.Options{
		BaseURL:  fake.URL(),
		Password: "secret",
		Timeout:  3 * time.Second,
		Retry:    retry.Config{Attempts: 3, Delay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	snap := c.FetchAll(context.Background())
	if !snap.Healthy() {
		t.Fatalf("一次瞬时 500 应被重试自愈，实际失败: %v", snap.FailedEndpoints())
	}
	if n := fake.Calls("api/misystem/status"); n < 2 {
		t.Errorf("status 被调用 %d 次，期望至少 2 次（首次失败 + 重试）", n)
	}
}

// TestConcurrentFetchAllIsRaceFree 在 -race 下并发采集，验证无数据竞争，
// 且并发请求被折叠（登录只发生一次）。
func TestConcurrentFetchAllIsRaceFree(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newClient(t, fake)

	var wg sync.WaitGroup
	snaps := make([]*router.Snapshot, 8)
	for i := range snaps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snaps[i] = c.FetchAll(context.Background())
		}(i)
	}
	wg.Wait()

	for i, s := range snaps {
		if s == nil || !s.Healthy() {
			t.Fatalf("第 %d 次并发采集失败", i)
		}
	}
	// 8 次并发采集共享同一次登录。
	if n := fake.LoginCalls(); n != 1 {
		t.Errorf("并发下登录次数 = %d，期望 1（singleflight 折叠生效）", n)
	}
}

// TestContextCancellation 不让挂起的路由器拖住整个调用方。
func TestContextCancellation(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c := newClient(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	snap := c.FetchAll(ctx)
	if snap == nil {
		t.Fatal("ctx 取消后仍应返回非 nil 快照")
	}
	if snap.Healthy() {
		t.Error("ctx 已取消，不应报告全部成功")
	}
}

// TestIsRetryable 表驱动覆盖重试判定。
func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context 取消", context.Canceled, false},
		{"context 超时", context.DeadlineExceeded, false},
		{"包装过的 ctx 取消", stderrors.New("wrap: " + context.Canceled.Error()), true},
		{"登录失败", router.ErrLoginFailed, true},
		{"HTTP 5xx", router.ErrHTTPStatus, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := router.IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestNewValidation 校验构造参数。
func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		opts    router.Options
		wantErr bool
	}{
		{"缺少 BaseURL", router.Options{Password: "x"}, true},
		{"缺少密码", router.Options{BaseURL: "http://1.1.1.1"}, true},
		{"参数齐全", router.Options{BaseURL: "http://1.1.1.1", Password: "x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := router.New(tc.opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New() error = %v，wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
