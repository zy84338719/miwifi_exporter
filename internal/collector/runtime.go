package collector

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
)

// runtimeMetrics 暴露 exporter 自身的运行时状态。
//
// 为什么只暴露三个值，而不是注册 client_golang 那套完整的 Go collector：
// 那会带进 go_* / process_* 几十条与路由器无关的序列，污染小型 Prometheus
// 的存储（main.go 里明确记了这个取舍）。这里只留能回答两个问题的最小集合：
//
//   - exporter 是不是在漏内存（heap_inuse / memory_sys 单调上涨）
//   - goroutine 是不是在堆积（poll 模式每轮起十个端点 goroutine）
//
// 这两个问题以前只能靠 ssh 上去看——而排查采集机的人未必有那台机器的 ssh。
type runtimeMetrics struct {
	goroutines prometheus.GaugeFunc
	heapInuse  prometheus.GaugeFunc
	memorySys  prometheus.GaugeFunc
}

// newRuntimeMetrics 构造运行时指标。
//
// 全部用 GaugeFunc：每次采集时现读，不需要维护状态，也不会忘更新。
func newRuntimeMetrics(ns string) *runtimeMetrics {
	const subsystem = "exporter"

	return &runtimeMetrics{
		goroutines: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: subsystem, Name: "goroutines",
			Help: "exporter 当前的 goroutine 数量（排查 goroutine 堆积）",
		}, func() float64 {
			return float64(runtime.NumGoroutine())
		}),
		heapInuse: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: subsystem, Name: "heap_inuse_bytes",
			Help: "exporter 堆内正在使用的字节数（排查内存泄漏）",
		}, func() float64 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return float64(m.HeapInuse)
		}),
		memorySys: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: subsystem, Name: "memory_sys_bytes",
			Help: "exporter 向系统申请的内存总量（Go runtime 视角）",
		}, func() float64 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return float64(m.Sys)
		}),
	}
}

// Register 把运行时指标注册进 registry。
func (m *runtimeMetrics) Register(reg *prometheus.Registry) error {
	for _, c := range []prometheus.Collector{m.goroutines, m.heapInuse, m.memorySys} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}
