package collector_test

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// TestPollSourceCachesSnapshot 验证 poll 模式：scrape 只读内存快照，
// 不再发起任何网络请求。
func TestPollSourceCachesSnapshot(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	client := newPollClient(t, fake)

	poll, err := collector.NewPollSource(collector.PollSourceOptions{
		Client: client, Interval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPollSource 失败: %v", err)
	}

	if poll.Ready() {
		t.Error("尚未开始轮询时不应为 Ready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poll.Run(ctx)

	// 等首轮采集落盘。
	deadline := time.Now().Add(3 * time.Second)
	for !poll.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !poll.Ready() {
		t.Fatal("3s 内未完成首轮采集")
	}

	before := fake.Calls("api/misystem/status")
	snap := poll.Collect(context.Background())
	if snap == nil || !snap.Healthy() {
		t.Fatalf("快照不健康: %+v", snap)
	}

	// 连续读 20 次，路由器的 status 接口调用次数不应增长。
	for i := 0; i < 20; i++ {
		poll.Collect(context.Background())
	}
	if after := fake.Calls("api/misystem/status"); after != before {
		t.Errorf("scrape 触发了网络请求：status 调用 %d → %d（应当只由后台轮询增长）", before, after)
	}
	if snap.Status.CPU.Core != 4 {
		t.Errorf("快照数据不正确: %+v", snap.Status.CPU)
	}
}

// TestPollSourceSurvivesRouterFailure 验证路由器挂掉时 poll 模式仍能返回快照。
func TestPollSourceSurvivesRouterFailure(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	client := newPollClient(t, fake)

	poll, err := collector.NewPollSource(collector.PollSourceOptions{
		Client: client, Interval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPollSource 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poll.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !poll.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// 路由器整个下线。
	fake.Server.Close()
	// 等后台至少跑完一轮：只有新一轮采集失败后，快照里才会带上失败标记。
	// 当前快照是下线**之前**落盘的，仍然是健康的。
	time.Sleep(100 * time.Millisecond)

	snap := poll.Collect(context.Background())
	if snap == nil {
		t.Fatal("路由器下线后 Collect 仍应返回非 nil 快照")
	}
	// 快照保留上一轮的数据（last-good），同时被标记为 stale。
	if snap.LanInfo.Info.Uptime == 0 {
		t.Error("路由器下线后应保留上一轮的 lan_info 值")
	}
	if st := snap.Endpoint["lan_info"]; st.OK || !st.Stale {
		t.Errorf("lan_info 应为失败且陈旧，实际 OK=%v Stale=%v", st.OK, st.Stale)
	}
	if snap.Healthy() {
		t.Error("路由器下线后快照不应报告全部健康")
	}
}

func TestPollSourceValidation(t *testing.T) {
	if _, err := collector.NewPollSource(collector.PollSourceOptions{}); err == nil {
		t.Error("缺少 Client 时应当报错")
	}
	fake := testutil.NewFakeRouter(t)
	client := newPollClient(t, fake)
	if _, err := collector.NewPollSource(collector.PollSourceOptions{Client: client}); err == nil {
		t.Error("间隔为 0 时应当报错")
	}
}

// TestCollectorPollModeEndToEnd 跑通 poll 模式下的完整指标暴露。
func TestCollectorPollModeEndToEnd(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	client := newPollClient(t, fake)

	poll, err := collector.NewPollSource(collector.PollSourceOptions{
		Client: client, Interval: time.Hour, // 只跑一轮，避免测试期间反复采集
	})
	if err != nil {
		t.Fatalf("NewPollSource 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poll.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !poll.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	c, err := collector.New(collector.CollectorOptions{
		Namespace: "miwifi",
		Host:      "testhost",
		Version:   "test",
		Source:    poll,
	})
	if err != nil {
		t.Fatalf("构造 Collector 失败: %v", err)
	}

	reg := gatherRegistry(t, c)
	families := gather(t, reg)

	if _, ok := families["miwifi_cpu_cores"]; !ok {
		t.Error("poll 模式下缺少路由器指标")
	}
	if v, ok := lookup(t, families, "miwifi_cpu_cores", map[string]string{"host": "testhost"}); !ok || v != 4 {
		t.Errorf("miwifi_cpu_cores = %v（存在=%v），期望 4", v, ok)
	}
	// mode 标签应为 poll。
	st := families["miwifi_exporter_scrape_duration_seconds"]
	if st == nil {
		t.Fatal("缺少 scrape_duration_seconds")
	}
	found := false
	for _, m := range st.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "mode" && lp.GetValue() == "poll" {
				found = true
			}
		}
	}
	if !found {
		t.Error("scrape_duration_seconds 的 mode 标签应为 poll")
	}
}

func newPollClient(t *testing.T, fake *testutil.FakeRouter) *router.Client {
	t.Helper()
	c, err := router.New(router.Options{
		BaseURL:  fake.URL(),
		Password: "pw",
		Timeout:  3 * time.Second,
		Retry:    retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 router.Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}
