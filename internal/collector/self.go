package collector

import (
	"runtime"
	"time"

	"github.com/oekit/logx"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/router"
)

// selfMetrics 是 exporter 自身的可观测指标。
//
// 这一族是本次重构新增的：旧版只有「路由器数据」，exporter 自身是否健康、
// 哪些端点在拖后腿、拿到的数据有多旧，完全看不出来。路由器一旦掉线，
// 最需要知道的就是这组数字。
//
// 命名统一为 <ns>_exporter_*，与路由器指标同 namespace、前缀区分，互不重名。
type selfMetrics struct {
	logger logx.Logger

	scrapeDuration    *prometheus.GaugeVec
	scrapeTotal       *prometheus.CounterVec
	endpointUp        *prometheus.GaugeVec
	endpointStale     *prometheus.GaugeVec
	endpointErrors    *prometheus.CounterVec
	endpointDur       *prometheus.GaugeVec
	endpointSupported *prometheus.GaugeVec
	snapshotAge       *prometheus.GaugeVec
	routerUp          *prometheus.GaugeVec
	// prof 用于在 Collect 时现算 endpoint_supported。
	//
	// 为什么不放在 Observe 里更新：Observe 是被**另一个** Collector
	// （Collector.Collect）调用的，注册表不保证两者的收集顺序，
	// 那样算出来的 gauge 会稳定滞后一拍——第一次抓取拿到的是初始值。
	prof *profile.Instance
	// host 是 host 标签值。多路由器时没有它就分不清是哪一台的问题。
	host string
	// mode 是采集模式（pull / poll），仅供异常路径记录用。
	mode string
}

// selfMetricOptions 是 selfMetrics 的构造参数。
type selfMetricOptions struct {
	Namespace string
	Mode      string
	Version   string
	Prof      *profile.Instance
	Host      string
	Logger    logx.Logger
}

// buildInfo 是 exporter 的构建信息。
//
// 它是**进程级事实**（Go 版本、构建版本），与采几台路由器无关，因此
// 放在顶层 collector 而不是每台的 selfMetrics 里——否则多机时每台各出
// 一份，报 `collected metric ... was collected before with the same name
// and label values`。standalone Collector 与 Set 各持有一份，
// 但同一个 registry 里只会注册其中一个。
type buildInfo struct {
	vec *prometheus.GaugeVec
}

func newBuildInfo(ns, version string) *buildInfo {
	v := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Subsystem: "exporter", Name: "build_info",
		Help: "exporter 构建信息，恒为 1",
	}, []string{"version", "go_version"})
	v.WithLabelValues(version, runtime.Version()).Set(1)
	return &buildInfo{vec: v}
}

func (b *buildInfo) Describe(ch chan<- *prometheus.Desc) { b.vec.Describe(ch) }

func (b *buildInfo) Collect(ch chan<- prometheus.Metric) { b.vec.Collect(ch) }

func newSelfMetrics(opts selfMetricOptions) *selfMetrics {
	ns := opts.Namespace

	logger := opts.Logger
	if logger == nil {
		logger = logx.Delegating("collector")
	}

	// 注意用 Subsystem="exporter" 而不是把 "exporter_" 拼进 Namespace：
	// prometheus.BuildFQName 用 "_" 连接非空的 namespace/subsystem/name，
	// 写成 Namespace:"miwifi_exporter_" 会得到 miwifi_exporter__router_up
	// 这种双下划线名字。
	const exporter = "exporter"

	m := &selfMetrics{
		logger: logger,
		scrapeDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: exporter, Name: "scrape_duration_seconds",
			Help: "最近一次采集耗时（秒）",
		}, []string{"host", "mode"}),
		scrapeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: exporter, Name: "scrape_total",
			Help: "采集触发次数，按结果分类",
		}, []string{"host", "mode", "result"}),
		endpointUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: exporter, Name: "endpoint_up",
			Help: "端点最近一次采集是否成功（1=成功，0=失败）",
		}, []string{"host", "endpoint"}),
		endpointStale: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: exporter, Name: "endpoint_stale",
			Help: "端点当前导出值是否为上一次成功留下的陈旧值（1/0）",
		}, []string{"host", "endpoint"}),
		endpointErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: exporter, Name: "endpoint_errors_total",
			Help: "端点采集失败累计次数",
		}, []string{"host", "endpoint"}),
		endpointDur: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: exporter, Name: "endpoint_duration_seconds",
			Help: "端点最近一次采集耗时（秒）",
		}, []string{"host", "endpoint"}),
		endpointSupported: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: exporter, Name: "endpoint_supported",
			Help: "当前固件 profile 是否认为本机提供该端点（1=提供，0=已跳过）",
		}, []string{"host", "endpoint"}),
		snapshotAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: exporter, Name: "snapshot_age_seconds",
			Help: "当前快照的年龄（秒）——poll 模式下即采集新鲜度",
		}, []string{"host"}),
		routerUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: exporter, Name: "router_up",
			Help: "该路由器最近一次采集是否可达（1/0）",
		}, []string{"host"}),
		prof: opts.Prof,
		host: opts.Host,
		mode: opts.Mode,
	}

	h := opts.Host
	// 让这些指标在第一次采集之前就存在于 /metrics 里。
	m.scrapeDuration.WithLabelValues(h, opts.Mode)
	for _, r := range []string{"success", "partial", "failure"} {
		m.scrapeTotal.WithLabelValues(h, opts.Mode, r)
	}
	for _, name := range profile.AllEndpoints {
		m.endpointUp.WithLabelValues(h, name).Set(0)
		m.endpointStale.WithLabelValues(h, name).Set(0)
		m.endpointErrors.WithLabelValues(h, name)
		m.endpointDur.WithLabelValues(h, name)
		m.endpointSupported.WithLabelValues(h, name).Set(1)
	}
	m.snapshotAge.WithLabelValues(h).Set(0)
	m.routerUp.WithLabelValues(h).Set(0)

	return m
}

// Collect 供 prometheus.Collector 接口使用。
// MarkUnreachable 把该路由器标成「采集异常中止」。
//
// 与「采到了但全是 0」区分开：后者会被误读成「各项指标真的都是 0」。
func (m *selfMetrics) MarkUnreachable() {
	m.routerUp.WithLabelValues(m.host).Set(0)
	m.scrapeTotal.WithLabelValues(m.host, m.mode, "failure").Inc()
}

func (m *selfMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.scrapeDuration.Describe(ch)
	m.scrapeTotal.Describe(ch)
	m.endpointUp.Describe(ch)
	m.endpointStale.Describe(ch)
	m.endpointErrors.Describe(ch)
	m.endpointDur.Describe(ch)
	m.endpointSupported.Describe(ch)
	m.snapshotAge.Describe(ch)
	m.routerUp.Describe(ch)
}

// Collect 供 prometheus.Collector 接口使用。
func (m *selfMetrics) Collect(ch chan<- prometheus.Metric) {
	// 在自己的 Collect 里刷新，保证本次抓取看到的就是当前 profile 状态。
	if m.prof != nil {
		for _, name := range profile.AllEndpoints {
			supported := 1.0
			if !m.prof.Enabled(name) {
				supported = 0
			}
			m.endpointSupported.WithLabelValues(m.host, name).Set(supported)
		}
	}

	m.scrapeDuration.Collect(ch)
	m.scrapeTotal.Collect(ch)
	m.endpointUp.Collect(ch)
	m.endpointStale.Collect(ch)
	m.endpointErrors.Collect(ch)
	m.endpointDur.Collect(ch)
	m.endpointSupported.Collect(ch)
	m.snapshotAge.Collect(ch)
	m.routerUp.Collect(ch)
}

// Observe 把一轮采集的结果写进自监控指标。
func (m *selfMetrics) Observe(mode string, elapsed time.Duration, snap *router.Snapshot) {
	h := m.host
	m.scrapeDuration.WithLabelValues(h, mode).Set(elapsed.Seconds())

	if snap == nil || len(snap.Endpoint) == 0 {
		m.scrapeTotal.WithLabelValues(h, mode, "failure").Inc()
		m.routerUp.WithLabelValues(h).Set(0)
		return
	}

	// router_up 的语义是「这台路由器我们采到数据了吗」，而不是
	// 「每一个端点都成功了吗」。
	//
	// 最初写成后者，真机上立刻暴露问题：一台 R3P 老机器不支持
	// pppoe_status，于是 7 个端点里 1 个失败就把整台判成不可达，
	// router_up 恒为 0、告警一直误报——而这台其实数据正常。
	// 端点级健康本来就有 endpoint_up{endpoint} 表达，不必再叠一层。
	anyOK := false
	for name, st := range snap.Endpoint {
		up, stale := 0.0, 0.0
		if st.OK {
			up = 1
			anyOK = true
		} else {
			m.endpointErrors.WithLabelValues(h, name).Inc()
		}
		if st.Stale {
			stale = 1
		}
		m.endpointUp.WithLabelValues(h, name).Set(up)
		m.endpointStale.WithLabelValues(h, name).Set(stale)
		m.endpointDur.WithLabelValues(h, name).Set(st.Elapsed.Seconds())
	}

	// scrape_total 的 success/partial 仍按「是否所有端点都成功」分档——
	// 它是给排障看的细节粒度，不用于告警，因此保留严格语义。
	result := "success"
	if len(snap.Endpoint) > 0 {
		allOK := true
		for _, st := range snap.Endpoint {
			if !st.OK {
				allOK = false
				break
			}
		}
		if !allOK {
			result = "partial"
		}
	}
	m.scrapeTotal.WithLabelValues(h, mode, result).Inc()

	// 告警口径：采到任意端点即视为可达。
	m.routerUp.WithLabelValues(h).Set(boolToFloat(anyOK))

	if !snap.CollectedAt.IsZero() {
		m.snapshotAge.WithLabelValues(h).Set(time.Since(snap.CollectedAt).Seconds())
	}
}
