package collector

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/oekit/concurrency/singleflight"
	"github.com/oekit/logx"
	oerr "github.com/oekit/pkg/errors"

	"github.com/helloworlde/miwifi-exporter/internal/router"
)

// PullSource 在每次 scrape 时同步向路由器取值。
//
// 这是默认模式，语义与旧版完全一致：指标值永远是抓取那一刻的真实值。
// 解决的不是「准不准」而是「慢不慢」——十个端点并发、每次调用都有超时、
// 并发 scrape 会被 singleflight 折叠成一次真实采集。
type PullSource struct {
	client  *router.Client
	timeout time.Duration
	logger  logx.Logger

	flight singleflight.Flight[*router.Snapshot]
	// last 保留最近一份快照。scrape 超时时宁可导出旧值，也不要让 /metrics 空掉。
	last atomic.Pointer[router.Snapshot]
}

// PullSourceOptions 是 PullSource 的构造参数。
type PullSourceOptions struct {
	Client  *router.Client
	Timeout time.Duration
	Logger  logx.Logger
}

// NewPullSource 构造 PullSource。
func NewPullSource(opts PullSourceOptions) *PullSource {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = logx.Delegating("collector")
	}
	return &PullSource{client: opts.Client, timeout: timeout, logger: logger}
}

// Mode 实现 Source。
func (p *PullSource) Mode() string { return "pull" }

// Collect 实现 Source。
//
// 超时兜底的取舍：ctx 超时意味着这次采集没跑完。此时返回上一次成功的快照并
// 打一条 warn，而不是返回空——Prometheus 抓取会立刻重试，把空结果写进时序库
// 只会在图上留下一段空洞。
func (p *PullSource) Collect(ctx context.Context) *router.Snapshot {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	snap, err := p.flight.Do(ctx, "pull", func(cctx context.Context) (*router.Snapshot, error) {
		return p.client.FetchAll(cctx), nil
	})
	if err != nil {
		p.logger.CtxError(ctx, "采集超时或被取消", "timeout", p.timeout, "err", err.Error())
		if prev := p.last.Load(); prev != nil {
			return prev
		}
		return emptySnapshot()
	}
	if snap != nil {
		p.last.Store(snap)
	}
	return snap
}

// PollSource 由后台 goroutine 周期采集，scrape 只读内存快照。
//
// 路由器掉线或变慢时，/metrics 依然秒回——代价是指标的新鲜度取决于
// COLLECT_INTERVAL 而不是抓取时刻。适合把 exporter 放在 K8s 集群里、
// 而路由器是家用宽带上的不稳定环节的场景。
type PollSource struct {
	interval time.Duration
	client   *router.Client
	logger   logx.Logger

	current atomic.Pointer[router.Snapshot]
	// ready 在首轮采集完成后置位，/readyz 据此判断。
	ready atomic.Bool
}

// PollSourceOptions 是 PollSource 的构造参数。
type PollSourceOptions struct {
	Client   *router.Client
	Interval time.Duration
	Logger   logx.Logger
}

// NewPollSource 构造 PollSource。
func NewPollSource(opts PollSourceOptions) (*PollSource, error) {
	if opts.Client == nil {
		return nil, oerr.NewBadRequestError("poll 模式需要提供路由器客户端")
	}
	if opts.Interval <= 0 {
		return nil, oerr.NewBadRequestError("poll 模式的采集间隔必须大于 0")
	}
	logger := opts.Logger
	if logger == nil {
		logger = logx.Delegating("collector")
	}
	return &PollSource{interval: opts.Interval, client: opts.Client, logger: logger}, nil
}

// Mode 实现 Source。
func (p *PollSource) Mode() string { return "poll" }

// Collect 实现 Source：只读快照，绝不发起网络请求。
func (p *PollSource) Collect(context.Context) *router.Snapshot {
	if snap := p.current.Load(); snap != nil {
		return snap
	}
	// 首轮还没跑完时 scrape：立刻同步采一次，避免 /readyz 之前的窗口里
	// /metrics 完全是空的。这条路径只会在启动初期命中。
	return p.client.FetchAll(context.Background())
}

// Ready 报告是否已经完成过至少一轮采集。
func (p *PollSource) Ready() bool { return p.ready.Load() }

// Run 是后台采集循环，由 bootstrap 的 GoBackground 托管，ctx 取消即退出。
func (p *PollSource) Run(ctx context.Context) {
	p.logger.Info("启动后台采集", "interval", p.interval)

	// 先立刻采一轮再进入 ticker 循环，否则启动后的第一个间隔内全是空快照。
	p.pollOnce(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("后台采集退出")
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

// pollOnce 执行一轮采集并落盘快照。
//
// 这里刻意不打印成败日志：Client 已经在 FetchAll 末尾汇总过一轮结果，
// 而且信息更全（逐端点错误 + 耗时）。两处都打的话，路由器宕机时每轮
// 会出现两条内容重复的 WARN。
//
// 「后台循环还活着吗」这个问题由 miwifi_exporter_snapshot_age_seconds 回答，
// 比翻日志可靠得多。
func (p *PollSource) pollOnce(ctx context.Context) {
	snap := p.client.FetchAll(ctx)
	p.current.Store(snap)
	p.ready.Store(true)
}

// emptySnapshot 是一份全零快照，用于「从未成功采集过」的场景。
//
// 导出全零而不是什么都不导出：Prometheus 的 staleness 判定基于「序列消失」，
// 空响应会被当作 target down，而全零值配合 exporter_router_up=0 语义更清楚。
func emptySnapshot() *router.Snapshot {
	return &router.Snapshot{CollectedAt: time.Now()}
}
