package collector_test

import (
	"context"
	"testing"
	"time"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// TestRuntimeMetricsExported exporter 自身的运行时状态必须能被看到。
//
// 以前没有这三个指标，于是「内存是不是在涨」「goroutine 是不是在堆积」
// 只能靠 ssh 上机器看——而排查采集机的人未必有那台机器的 ssh。
//
// 刻意不注册 client_golang 那套完整的 Go collector：它会带进 go_*/process_*
// 几十条与路由器无关的序列，污染小型 Prometheus 的存储。
func TestRuntimeMetricsExported(t *testing.T) {
	fake := testutil.NewFakeRouter(t)

	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, fake, "r1", 5*time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	fams := gatherSet(t, set)

	for _, name := range []string{
		"miwifi_exporter_goroutines",
		"miwifi_exporter_heap_inuse_bytes",
		"miwifi_exporter_memory_sys_bytes",
	} {
		if _, ok := fams[name]; !ok {
			t.Errorf("缺少运行时指标 %s", name)
		}
	}

	if v := mustLookup(t, fams, "miwifi_exporter_goroutines", nil); v <= 0 {
		t.Errorf("goroutines = %v，期望 > 0", v)
	}
	if v := mustLookup(t, fams, "miwifi_exporter_heap_inuse_bytes", nil); v <= 0 {
		t.Errorf("heap_inuse_bytes = %v，期望 > 0", v)
	}
	if v := mustLookup(t, fams, "miwifi_exporter_memory_sys_bytes", nil); v <= 0 {
		t.Errorf("memory_sys_bytes = %v，期望 > 0", v)
	}
}
