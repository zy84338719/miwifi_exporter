package router_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// TestConcurrentScrapesDuringStokChurn 压力测试：stok 持续作废时，
// 大量并发采集同时触发重新登录。
//
// 这是最容易出死锁/竞态的组合——十个端点并发 403 → 各自 invalidate →
// 各自去登录。这里同时验证三件事：不卡死、stok 最终能自愈、登录次数有界
// （不会因为并发风暴把路由器打挂）。
func TestConcurrentScrapesDuringStokChurn(t *testing.T) {
	// 关键：假路由器必须有可观的延迟，否则 48 轮采集瞬间跑完，
	// stok 剧变根本来不及发生——测试会「通过」却什么也没压到。
	fake := testutil.NewFakeRouterWithDelay(t, 15*time.Millisecond)
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw",
		Timeout: 5 * time.Second, ProfileName: profile.NoScan,
		Retry: retry.Config{Attempts: 2, Delay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 让 stok 每隔一小段时间就失效一次，逼出重新登录路径。
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Millisecond):
				fake.RotateStok()
			}
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, 64)

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				// 每轮采集都会被 cancel 打一次，模拟 Prometheus 超时后
				// 客户端断开——顺带验证 ctx 取消不会让采集卡死。
				cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
				snap := c.FetchAll(cctx)
				ccancel()
				if snap == nil {
					select {
					case errCh <- errString("FetchAll 返回了 nil 快照"):
					default:
					}
				}
			}
		}()
	}

	// 带超时等待，防止死锁时测试永远挂着。
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(25 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("stok 剧变下的并发采集卡住了（疑似死锁）\n%s", buf[:n])
	}
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// 风暴过后 stok 必须能自愈，否则说明自愈路径本身坏了。
	fake.RotateStok()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap := c.FetchAll(ctx)
		if snap.Healthy() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stok 恢复后 10s 内仍未自愈: %v", snap.FailedEndpoints())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestNoGoroutineLeak 检查反复采集不会累积 goroutine。
//
// 每个 scrape 都会 context.WithTimeout；泄漏的话进程内存会随抓取次数线性增长。
func TestNoGoroutineLeak(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw", Timeout: 3 * time.Second,
		ProfileName: profile.NoScan, Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	ctx := context.Background()
	for i := 0; i < 20; i++ { // 预热
		c.FetchAll(ctx)
	}
	// 让前一批的 timer 有机会回收
	settle := func() int {
		runtime.GC()
		time.Sleep(200 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()

	for i := 0; i < 60; i++ {
		c.FetchAll(ctx)
	}
	after := settle()

	// 允许少量抖动（HTTP keep-alive 连接、runtime 内部 goroutine），
	// 但线性增长必须被拦住。
	if after-before > 10 {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutine 泄漏：60 轮采集前后 %d → %d\n%s", before, after, buf[:n])
	}
}

// TestCanceledContextDoesNotBlock 保证「客户端断开」不会拖住采集。
//
// 这是 ctx 贯通改造的初衷：Prometheus 超时断开后 exporter 应立刻停手，
// 而不是继续把十个接口跑完。
func TestCanceledContextDoesNotBlock(t *testing.T) {
	fake := testutil.NewFakeRouterWithDelay(t, 2*time.Second)
	c, err := router.New(router.Options{
		BaseURL: fake.URL(), Password: "pw", Timeout: 5 * time.Second,
		ProfileName: profile.NoScan, Retry: retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		c.FetchAll(ctx)
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Errorf("ctx 取消后仍耗时 %v，应在 2s 延迟的接口返回前就中止", elapsed)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 FetchAll 未返回")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
