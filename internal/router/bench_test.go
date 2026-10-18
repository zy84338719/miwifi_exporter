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

// BenchmarkFetchAll 量化「十个端点并发」相对串行的收益。
//
// 每个端点在服务端固定 sleep 固定时长，因此串行耗时应约为
// 并发的 numEndpoints 倍——这不是为了好看，而是把设计意图固化成可回归的数字。
// 真实路由器上这个差距由 RTT 决定，家用路由器 RTT 往往还不小。
func BenchmarkFetchAll(b *testing.B) {
	const perEndpointDelay = 20 * time.Millisecond

	fake := testutil.NewFakeRouterWithDelay(b, perEndpointDelay)
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw", Timeout: 10 * time.Second,
		Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		b.Fatalf("构造 Client 失败: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		snap := c.FetchAll(ctx)
		if !snap.Healthy() {
			b.Fatalf("采集不健康: %v", snap.FailedEndpoints())
		}
	}
}

// TestFetchAllLatencyBound 固化并发行为：十��端点各延迟 20ms，
// 一轮采集应当接近 20ms 而不是 200ms。这条是断言而不只是基准，
// 因为它直接对应用户能感知到的「scrape 会不会超时」。
func TestFetchAllLatencyBound(t *testing.T) {
	const perEndpointDelay = 20 * time.Millisecond

	fake := testutil.NewFakeRouterWithDelay(t, perEndpointDelay)
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw", Timeout: 10 * time.Second,
		Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	start := time.Now()
	snap := c.FetchAll(context.Background())
	elapsed := time.Since(start)

	if !snap.Healthy() {
		t.Fatalf("采集不健康: %v", snap.FailedEndpoints())
	}

	serial := perEndpointDelay * time.Duration(len(profile.AllEndpoints))
	// 并发的收益会因调度开销打些折扣，留一倍余量仍足够严格：
	// 串行是 200ms，阈值 100ms 只可能被真正并发满足。
	threshold := serial / 2
	if elapsed > threshold {
		t.Errorf("采集耗时 %v 超过阈值 %v——十个端点没有真正并发（串行应约 %v）",
			elapsed, threshold, serial)
	}
	t.Logf("十端点各 %v：实测 %v（串行约 %v）", perEndpointDelay, elapsed, serial)
}
