package collector_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// TestConcurrentScrapesCollapseToOneCollection 验证并发抓取被折叠成一次采集。
//
// 抓取级 singleflight 住在 Collector 里，不在 router.Client 里——这正是
// 分层的目的：Client 只管「取一份数据」，「多个人同时要同一份数据时
// 只取一次」是上层的责任。
//
// 不折叠的代价很实在：Prometheus 对同一 target 并发拉取时，十个接口
// 会被成倍放大，对一台家用路由器是实打实的负载。
func TestConcurrentScrapesCollapseToOneCollection(t *testing.T) {
	const perEndpointDelay = 30 * time.Millisecond

	fake := testutil.NewFakeRouterWithDelay(t, perEndpointDelay)
	client := newDelayClient(t, fake, 10*time.Second)

	c, err := collector.New(collector.CollectorOptions{
		Namespace: "miwifi", Host: "testhost", Version: "test",
		Source: collector.NewPullSource(collector.PullSourceOptions{
			Client: client, Timeout: 10 * time.Second,
		}),
	})
	if err != nil {
		t.Fatalf("构造 Collector 失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := reg.Register(c.Self()); err != nil {
		t.Fatalf("注册自监控指标失败: %v", err)
	}

	// 先跑一轮把登录做掉，后续的调用计数才有可比性。
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("首次 Gather 失败: %v", err)
	}
	baseline := fake.Calls("api/misystem/status")

	const parallel = 8
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Gather(); err != nil {
				t.Errorf("并发 Gather 失败: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 一轮采集约等于最慢的端点；8 路并发若真发 8 轮，至少要 8 倍。
	if elapsed > perEndpointDelay*4 {
		t.Errorf("8 路并发耗时 %v，看起来并没有折叠成一次采集", elapsed)
	}

	extra := fake.Calls("api/misystem/status") - baseline
	if extra > 2 {
		t.Errorf("8 路并发触发了 %d 次 status 请求，应被折叠到 1 次左右", extra)
	}
	t.Logf("8 路并发：status 额外请求 %d 次，总耗时 %v", extra, elapsed)
}

// BenchmarkCollectParallel 量化「折叠」给路由器省下多少请求。
func BenchmarkCollectParallel(b *testing.B) {
	fake := testutil.NewFakeRouterWithDelay(b, time.Millisecond)
	client := newDelayClientB(b, fake, 10*time.Second)

	c, err := collector.New(collector.CollectorOptions{
		Namespace: "miwifi", Host: "testhost", Version: "test",
		Source: collector.NewPullSource(collector.PullSourceOptions{
			Client: client, Timeout: 10 * time.Second,
		}),
	})
	if err != nil {
		b.Fatalf("构造 Collector 失败: %v", err)
	}
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		b.Fatalf("注册失败: %v", err)
	}
	if err := reg.Register(c.Self()); err != nil {
		b.Fatalf("注册自监控指标失败: %v", err)
	}

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.SnapshotFor(ctx); err != nil {
			b.Fatalf("采集失败: %v", err)
		}
	}
	b.StopTimer()

	total := 0
	for _, ep := range []string{"api/misystem/status", "api/xqnetwork/wan_info"} {
		total += fake.Calls(ep)
	}
	b.ReportMetric(float64(total)/float64(b.N), "requests/iter")
}

func newDelayClient(t *testing.T, fake *testutil.FakeRouter, timeout time.Duration) *router.Client {
	t.Helper()
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw", Timeout: timeout,
		Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 router.Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func newDelayClientB(b *testing.B, fake *testutil.FakeRouter, timeout time.Duration) *router.Client {
	b.Helper()
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw", Timeout: timeout,
		Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		b.Fatalf("构造 router.Client 失败: %v", err)
	}
	b.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}
