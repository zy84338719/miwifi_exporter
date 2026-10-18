package collector

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/logx"
	oerr "github.com/oekit/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/router"
)

// ModePoll 是后台轮询模式名，与 config.ModePoll 取值一致。
// 这里不复用 config 包是为了让 collector 不依赖配置层。
const ModePoll = "poll"

// RouterSpec 描述一台待接入的路由器。
type RouterSpec struct {
	// Host 是 host 标签取值，也是实例标识，必须全局唯一。
	Host string
	// IP 是原始地址，仅用于状态展示。
	IP string
	// BaseURL 是已归一化的路由器根地址（如 http://192.168.31.1）。
	// 地址归一化由配置层负责——采集层没必要知道「IPv6 要补方括号」这种事。
	BaseURL  string
	Password string

	// 以下都是按台覆盖。
	Profile          string
	Timeout          time.Duration
	Mode             string
	Interval         time.Duration
	ExcludeEndpoints []string
	EndpointTimeouts map[string]time.Duration
	TimeoutThreshold int
}

// Instance 是一台路由器对应的完整采集链路。
//
// 每台各自持有 client（stok、熔断、自适应降级状态都是独立的）、source 与
// collector，**互不共享任何可变状态**。这是多机场景最关键的一条：一台路由器
// 的登录失效或熔断打开，不应让另一台跟着降级。
type Instance struct {
	Spec    RouterSpec
	Client  *router.Client
	Source  Source
	Metrics *Collector
	poller  *PollSource
}

// Set 是**唯一**注册进 registry 的 collector，持有全部实例。
//
// 为什么不是「每台一个 Collector 各自注册」：prometheus.Registry 按 Desc
// 的**内容**（fqName + 标签名）判重，不看指针。两个 descs 完全相同的
// Collector 会被判为重复收集器，第二个注册直接报
// `duplicate metrics collector registration attempted`。
//
// 所以这里反过来：所有实例共享同一份 descs，Set 自己实现
// Describe/Collect，内部逐台扇出。一次 Describe、N 次采集。
type Set struct {
	instances []*Instance
	descs     *descs
	build     *buildInfo
	logger    logx.Logger

	reqCtx atomic.Pointer[setRequestCtx]
}

// setRequestCtx 包装 ctx 存进 atomic.Pointer。
type setRequestCtx struct{ ctx context.Context }

// SetOptions 是 NewSet 的可选参数。
type SetOptions struct {
	// Namespace 是全部实例共用的指标前缀。
	Namespace string
	Version   string
	Logger    logx.Logger
}

// NewSet 按规格构造全部实例。
//
// 构造期把任何一台的问题直接返回，而不是留到第一次抓取才炸——
// 配置错了应该在启动时就说清楚是哪一台的什么问题。
func NewSet(specs []RouterSpec, opts SetOptions) (*Set, error) {
	if len(specs) == 0 {
		return nil, oerr.NewBadRequestError("至少要配置一台路由器")
	}
	logger := opts.Logger
	if logger == nil {
		logger = logx.Delegating("collector")
	}

	seen := make(map[string]bool, len(specs))
	// 所有实例共享同一份描述符——指标契约对每台是一样的，差的只有标签取值。
	sharedDescs := newDescs(opts.Namespace)
	set := &Set{
		logger: logger,
		descs:  sharedDescs,
		build:  newBuildInfo(opts.Namespace, opts.Version),
	}

	for i, spec := range specs {
		if spec.Host == "" {
			return nil, oerr.NewBadRequestError("第 " + itoa(i+1) + " 台路由器缺少 host 标签")
		}
		if seen[spec.Host] {
			return nil, oerr.NewBadRequestError(
				"host 标签重复：" + spec.Host + "（两台路由器的指标会在标签上撞车）")
		}
		seen[spec.Host] = true

		inst, err := newInstance(spec, opts, i, sharedDescs)
		if err != nil {
			return nil, err
		}
		set.instances = append(set.instances, inst)
	}

	logger.Info("已接入路由器", "count", len(set.instances), "hosts", hostsOf(specs))
	return set, nil
}

func newInstance(spec RouterSpec, opts SetOptions, idx int, d *descs) (*Instance, error) {
	instanceLogger := logx.Delegating("router:" + spec.Host)

	client, err := router.New(router.Options{
		BaseURL:                  spec.BaseURL,
		Password:                 spec.Password,
		Timeout:                  spec.Timeout,
		ProfileName:              spec.Profile,
		ExcludeEndpoints:         spec.ExcludeEndpoints,
		EndpointTimeouts:         spec.EndpointTimeouts,
		EndpointTimeoutThreshold: spec.TimeoutThreshold,
		Logger:                   instanceLogger,
	})
	if err != nil {
		return nil, oerr.Wrap(err, "初始化路由器失败", "host", spec.Host, "index", idx+1)
	}

	var (
		source Source
		poller *PollSource
	)
	if spec.Mode == ModePoll {
		p, err := NewPollSource(PollSourceOptions{
			Client: client, Interval: spec.Interval,
			Logger: logx.Delegating("collector:" + spec.Host),
		})
		if err != nil {
			_ = client.Close(context.Background())
			return nil, oerr.Wrap(err, "初始化 poll 采集失败", "host", spec.Host)
		}
		source, poller = p, p
	} else {
		source = NewPullSource(PullSourceOptions{
			Client: client, Timeout: spec.Timeout,
			Logger: logx.Delegating("collector:" + spec.Host),
		})
	}

	metrics, err := New(CollectorOptions{
		Namespace: opts.Namespace,
		Host:      spec.Host,
		Source:    source,
		Profile:   client.Profile(),
		Version:   opts.Version,
		Descs:     d, // 共享给所有实例
		Logger:    logx.Delegating("collector:" + spec.Host),
	})
	if err != nil {
		_ = client.Close(context.Background())
		return nil, oerr.Wrap(err, "初始化采集器失败", "host", spec.Host)
	}

	return &Instance{Spec: spec, Client: client, Source: source, Metrics: metrics, poller: poller}, nil
}

// Instances 返回全部实例（顺序与配置一致）。
func (s *Set) Instances() []*Instance { return s.instances }

// Register 把整个 Set（而不是逐台 Collector）注册进 registry。
func (s *Set) Register(reg *prometheus.Registry) error {
	if err := reg.Register(s); err != nil {
		return oerr.Wrap(err, "注册指标失败")
	}
	return nil
}

// Describe 实现 prometheus.Collector：所有实例共用一份描述符，声明一次。
func (s *Set) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range s.descs.all() {
		ch <- d
	}
	for _, inst := range s.instances {
		inst.Metrics.Self().Describe(ch)
	}
	s.build.Describe(ch)
}

// Collect 实现 prometheus.Collector：逐台并发采集，再统一编码输出。
//
// 各台之间没有共享可变状态，一台超时/熔断只影响它自己的那一组样本。
func (s *Set) Collect(ch chan<- prometheus.Metric) {
	ctx := s.takeRequestContext()

	snaps := make([]*router.Snapshot, len(s.instances))
	done := make([]bool, len(s.instances))
	var wg sync.WaitGroup
	for i, inst := range s.instances {
		wg.Add(1)
		// 禁裸 go func：某个实例采集出意外 panic 时，不该带走其余 N-1 台。
		//
		// 但 async 会 **recover** 掉 panic，于是这里必须自己判断「有没有跑完」。
		// 否则一次 panic 会让那台路由器的指标**静默消失**，而 /metrics 照样
		// 返回 200——这种故障能潜伏很久才被发现（实测踩过：
		// endpointErrors 少传一个 host 导致 panic，整台的指标全没了，
		// 而所有测试都是绿的）。
		async.GoSafeWithContextLabeled(ctx, "collect:"+inst.Spec.Host, func(gctx context.Context) {
			defer wg.Done()
			defer func() { done[i] = true }()
			snap, _ := inst.Metrics.SnapshotFor(gctx)
			snaps[i] = snap
		})
	}
	wg.Wait()

	for i, inst := range s.instances {
		if !done[i] {
			// goroutine 都没跑到 defer，说明 panic 发生在更外层。
			s.logger.Error("该路由器的采集 goroutine 异常中止，本轮无数据",
				"host", inst.Spec.Host)
		}
		if snaps[i] == nil {
			// 明确把它标成不可用，别让「没数据」和「没采到」混为一谈。
			inst.Metrics.MarkUnreachable()
			continue
		}
		inst.Metrics.Emit(ch, snaps[i])
	}
	// 自监控指标在所有实例都采集完之后统一输出，保证与本次数据同源。
	for _, inst := range s.instances {
		inst.Metrics.Self().Collect(ch)
	}
	s.build.Collect(ch)
}

// StartBackground 启动所有 poll 实例的后台采集。
//
// 每台一个 goroutine，互不阻塞：一台卡住不影响另一台攒快照。
func (s *Set) StartBackground(app backgroundApp) {
	for _, inst := range s.instances {
		if inst.poller == nil {
			continue
		}
		p, host := inst.poller, inst.Spec.Host
		app.GoBackground("collector:"+host, func(ctx context.Context) { p.Run(ctx) })
	}
}

// backgroundApp 是 bootstrap.App 里本包需要的那一小部分。
type backgroundApp interface {
	GoBackground(name string, fn func(ctx context.Context))
}

// Close 关闭全部客户端连接。幂等。
func (s *Set) Close(ctx context.Context) error {
	var firstErr error
	for _, inst := range s.instances {
		if err := inst.Client.Close(ctx); err != nil && firstErr == nil {
			firstErr = oerr.Wrap(err, "关闭客户端失败", "host", inst.Spec.Host)
		}
	}
	return firstErr
}

// Ready 实现 server.Options.ReadyCheck：所有实例都就绪才返回 nil。
//
// 判「全部」而不是「任一」：多机场景下只报一台正常会掩盖另一台的问题。
// pull 模式没有前置条件，恒就绪。
func (s *Set) Ready(ctx context.Context) error {
	_ = ctx // 探针的 ctx 在此不参与判定：就绪与否是采集侧的状态，不由调用方决定
	return s.ready()
}

func (s *Set) ready() error {
	for _, inst := range s.instances {
		if inst.poller == nil {
			continue
		}
		if !inst.poller.Ready() {
			return oerr.NewBadRequestError("等待路由器 " + inst.Spec.Host + " 的首轮后台采集完成")
		}
	}
	return nil
}

// RouterStatus 是一台路由器的运行状态，供 /routers 端点与日志展示。
type RouterStatus struct {
	Host      string   `json:"host"`
	IP        string   `json:"ip"`
	Profile   string   `json:"profile"`
	Mode      string   `json:"mode"`
	Ready     bool     `json:"ready"`
	Endpoints []string `json:"enabledEndpoints"`
	Disabled  []string `json:"disabledEndpoints"`
	Timeouts  int64    `json:"timeouts"`
	// Schema 标记这台导出的端点集合是否与其余各台一致，见 SchemaDivergence。
	SchemaAligned bool `json:"schemaAligned"`
}

// SchemaDivergence 检查各台启用的端点集合是否一致，返回不一致的说明。
//
// 为什么需要它：profile 的**唯一**全局效果是决定采哪些接口，而接口集合
// 直接决定导出的指标集合。给 A 台配 default、给 B 台配 no-scan 之后，
// A 台的 neighbor_wifi_* 会消失（因为 wifi_list 被跳过），B 台的却还在。
//
// 更糟的是本次实测发现的连带效应：真机验证时 A 台用 default
// （含 wifi_list），WiFi 扫描阻塞了整机，导致**九个端点全挂**，
// router_up=0——同一台机器换回 no-scan 就 9/9 全通。
//
// 也就是说「不同路由器用不同 profile」不只是 schema 不一致，
// 还可能让其中几台彻底采不到数据。
func (s *Set) SchemaDivergence() []string {
	if len(s.instances) < 2 {
		return nil
	}
	base := s.instances[0].Client.Profile().EnabledEndpoints()
	baseSet := toSet(base)

	var warnings []string
	for _, inst := range s.instances[1:] {
		got := inst.Client.Profile().EnabledEndpoints()
		gotSet := toSet(got)

		var missing, extra []string
		for _, ep := range got {
			if !baseSet[ep] {
				missing = append(missing, ep)
			}
		}
		for _, ep := range base {
			if !gotSet[ep] {
				extra = append(extra, ep)
			}
		}
		if len(missing) == 0 && len(extra) == 0 {
			continue
		}
		warnings = append(warnings, formatDivergence(
			s.instances[0].Spec.Host, inst.Spec.Host,
			s.instances[0].Client.Profile().Name(), inst.Client.Profile().Name(),
			missing, extra))
	}
	return warnings
}

func formatDivergence(hostA, hostB, profileA, profileB string, missing, extra []string) string {
	var parts []string
	if len(extra) > 0 {
		parts = append(parts, "相比 "+hostA+" 缺少: "+joinStr(extra))
	}
	if len(missing) > 0 {
		parts = append(parts, "相比 "+hostA+" 多出: "+joinStr(missing))
	}
	return hostA + "(" + profileA + ") 与 " + hostB + "(" + profileB + ") 采集的接口集合不一致 —— " +
		joinStr(parts) + "；两台导出的指标集合会不同，建议统一 profile"
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

func joinStr(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := items[0]
	for _, s := range items[1:] {
		out += ", " + s
	}
	return out
}

// SchemaWarnings 返回需要写进日志的 schema 一致性告警。
func (s *Set) SchemaWarnings() []string { return s.SchemaDivergence() }

// schemaAligned 判断某台的端点集合是否与第一台一致。
//
// 第一台作为基准而不是「多数派」：配置里 routers 数组的第一项通常就是
// 基准那台，用它做参照最符合直觉，也不会因为顺序抖动而变化。
func (s *Set) schemaAligned(host string) bool {
	if len(s.instances) < 2 {
		return true
	}
	base := toSet(s.instances[0].Client.Profile().EnabledEndpoints())
	for _, inst := range s.instances {
		if inst.Spec.Host != host {
			continue
		}
		got := toSet(inst.Client.Profile().EnabledEndpoints())
		if len(base) != len(got) {
			return false
		}
		for ep := range base {
			if !got[ep] {
				return false
			}
		}
		return true
	}
	return true
}

// SetRequestContext 把抓取请求的 context 注入全部实例。
//
// 多机场景下必须扇出到每一台：只注入一台的话，其余 N-1 台会拿
// context.Background() 去采集，Prometheus 超时断开后它们仍在打路由器。
func (s *Set) SetRequestContext(ctx context.Context) {
	s.reqCtx.Store(&setRequestCtx{ctx: ctx})
}

// takeRequestContext 取一次请求 ctx，取后清空。
func (s *Set) takeRequestContext() context.Context {
	rc := s.reqCtx.Swap(nil)
	if rc == nil || rc.ctx.Err() != nil {
		return context.Background()
	}
	return rc.ctx
}

// Status 返回逐台状态快照。
func (s *Set) Status() []RouterStatus {
	out := make([]RouterStatus, 0, len(s.instances))
	for _, inst := range s.instances {
		st := RouterStatus{
			Host:          inst.Spec.Host,
			IP:            inst.Spec.IP,
			Profile:       inst.Client.Profile().Name(),
			Mode:          inst.Source.Mode(),
			Ready:         true,
			Endpoints:     inst.Client.Profile().EnabledEndpoints(),
			Disabled:      inst.Client.Profile().DisabledEndpoints(),
			Timeouts:      inst.Client.Profile().TotalTimeouts(),
			SchemaAligned: s.schemaAligned(inst.Spec.Host),
		}
		if inst.poller != nil {
			st.Ready = inst.poller.Ready()
		}
		out = append(out, st)
	}
	return out
}

func hostsOf(specs []RouterSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Host)
	}
	return out
}

func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
