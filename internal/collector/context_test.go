package collector_test

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// blockingSource 是一个「采集会一直卡住」的假采集链路，用来观察 ctx 是否传到位。
type blockingSource struct {
	started  chan struct{}
	entered  chan struct{} // 收到 ctx 后关闭
	canceled chan struct{} // ctx 被取消后关闭
	once     bool
}

func newBlockingSource() *blockingSource {
	return &blockingSource{
		started:  make(chan struct{}),
		entered:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
}

func (b *blockingSource) Mode() string { return "pull" }

func (b *blockingSource) Collect(ctx context.Context) *router.Snapshot {
	close(b.started)
	close(b.entered)
	<-ctx.Done()
	close(b.canceled)
	return &router.Snapshot{CollectedAt: time.Now()}
}

// TestRequestContextCancelsCollection 验证请求 ctx 真的传到了采集链路。
//
// 没有这个 ctx 时，Prometheus 到了 scrape_timeout 会直接断开，
// 而 exporter 仍然会把十个接口老老实实跑完——没人要的结果，白压路由器。
func TestRequestContextCancelsCollection(t *testing.T) {
	src := newBlockingSource()
	c, err := collector.New(collector.CollectorOptions{
		Namespace: "miwifi", Host: "h", Version: "test", Source: src,
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

	ctx, cancel := context.WithCancel(context.Background())
	c.SetRequestContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = reg.Gather()
	}()

	<-src.started
	select {
	case <-src.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("采集没有被触发")
	}

	cancel() // 模拟 Prometheus 超时断开

	select {
	case <-src.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("请求 ctx 取消后采集没有随之中止")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Gather 未能在 ctx 取消后返回")
	}
}

// TestClientDisconnectDoesNotPolluteRouterHealth 验证「客户端走了」不算路由器故障。
//
// 否则一次 Prometheus 超时就会把 router_up 打成 0，告警误报；
// 指标应当保持上一次已知状态。
func TestClientDisconnectDoesNotPolluteRouterHealth(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	client, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw",
		Timeout: 3 * time.Second, Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	c, err := collector.New(collector.CollectorOptions{
		Namespace: "miwifi", Host: "testhost", Version: "test",
		Source: collector.NewPullSource(collector.PullSourceOptions{
			Client: client, Timeout: 5 * time.Second,
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

	// 先来一次正常抓取，把 router_up 置为 1。
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("首次 Gather 失败: %v", err)
	}
	if v, ok := lookup(t, gather(t, reg), "miwifi_exporter_router_up", nil); !ok || v != 1 {
		t.Fatalf("首次抓取后 router_up = %v（存在=%v），期望 1", v, ok)
	}

	// 再来一次「客户端已取消」的抓取。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.SetRequestContext(ctx)

	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	if v, ok := lookup(t, gather(t, reg), "miwifi_exporter_router_up", nil); !ok || v != 1 {
		t.Errorf("客户端取消抓取后 router_up = %v（存在=%v），应保持 1 不受客户端行为影响", v, ok)
	}
}

// TestStaleRequestContextIsIgnored 守住「取用即失效」。
//
// 万一某次注入的 ctx 没被 Collect 取走（比如路由注册变了导致 handler 没走到），
// 下一次 Collect 绝不能拿那个已取消的 ctx 去采集。
func TestStaleRequestContextIsIgnored(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	client, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw",
		Timeout: 3 * time.Second, Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	c, err := collector.New(collector.CollectorOptions{
		Namespace: "miwifi", Host: "testhost", Version: "test",
		Source: collector.NewPullSource(collector.PullSourceOptions{
			Client: client, Timeout: 5 * time.Second,
		}),
	})
	if err != nil {
		t.Fatalf("构造 Collector 失败: %v", err)
	}

	// 注入一个立刻取消的 ctx，但**不**让它紧接着被 Collect 用掉：
	// 中间插一次成功的 Gather，模拟 handler 与 Collect 之间的时序错位。
	ctx, cancel := context.WithCancel(context.Background())
	c.SetRequestContext(ctx)
	cancel()

	if _, err := c.SnapshotFor(context.Background()); err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	snap, err := c.SnapshotFor(context.Background())
	if err != nil {
		t.Fatalf("第二次采集失败: %v", err)
	}
	if snap == nil || !snap.Healthy() {
		t.Errorf("陈旧的已取消 ctx 不应影响后续采集，实际: %+v", snap)
	}
}
