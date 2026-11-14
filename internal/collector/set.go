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

	"github.com/helloworlde/miwifi-exporter/internal/profile"
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
	// EndpointErrors 是「端点名 → 最近一次失败原因」，只含本轮失败的端点。
	//
	// 为什么必须挂在 HTTP 上而不只留在日志里：指标能告诉你
	// endpoint_up{endpoint="pppoe_status"} 0，但说不出为什么。真实案例是
	// 一台 R3P 上 pppoe_status 反复失败、scrape_total 永远只有 partial，
	// 而原因只出现在容器日志里——排查的人却未必有那台机器的 ssh。
	// 一条 curl 拿到错误文本，比翻日志快一个数量级。
	EndpointErrors map[string]string `json:"endpointErrors,omitempty"`
	// SchemaAligned 标记这台导出的端点集合是否**不比基准台少**，见 schemaAligned。
	//
	// 语义在 1.19.3 起调整过：早先它表示「与第一台完全一致」，
	// 于是不同型号的路由器必然为 false——而把两台强行对齐的唯一办法
	// 是排除掉真实存在的数据（把支持 newstatus 的那台也排掉，
	// 连在线终端数 wifi_sta_count 一起丢掉）。
	// 现在它表达的是「跨台对比时这台会不会少东西」，这才是真正要紧的问题。
	SchemaAligned bool `json:"schemaAligned"`
	// SchemaDiff 列出这台与基准台之间的端点差异及原因，仅在有差异时出现。
	//
	// 为什么需要它：schemaAligned 只能回答「有没有缺口」，
	// 但排查时更想知道「差在哪、为什么差」——是固件本来就没有，
	// 还是这台坏了。两者的处置完全相反。
	SchemaDiff []string `json:"schemaDiff,omitempty"`
}

// SchemaDivergence 返回各台之间的全部端点集合差异说明（不分成因）。
//
// 想知道「差在哪」看它；想知道「哪些需要人处理」看 SchemaWarnings；
// 想知道「哪些只是固件能力不同」看 SchemaNotes。
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
	var out []string
	for _, d := range s.schemaDiffs() {
		out = append(out, d.Detail())
	}
	return out
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

// 端点集合差异的三种成因。
//
// 分成三类是因为处置方式完全不同：配置差异要人改配置，
// 故障差异要人修机器，而固件差异**什么都不用做**——那台机器本来就没有
// 这个接口，把它当成问题去「统一」只会白白丢掉真实存在的数据。
const (
	// kindConfig 表示差异来自人为配置（profile 选得不一样、显式排除端点）。
	kindConfig = "config"
	// kindFirmware 表示差异来自固件能力（这台压根不提供该接口）。
	kindFirmware = "firmware"
	// kindFault 表示差异来自故障（连续失败被停采）。
	kindFault = "fault"
)

// schemaDiff 描述两台路由器之间的一个端点集合差异。
type schemaDiff struct {
	BaseHost     string
	BaseProfile  string
	OtherHost    string
	OtherProfile string
	// MissingHost 是缺少该端点的那台。
	MissingHost string
	Endpoint    string
	Reason      string
	Kind        string
}

// Detail 返回给人看的一行说明。
func (d schemaDiff) Detail() string {
	return d.BaseHost + "(" + d.BaseProfile + ") 与 " + d.OtherHost + "(" + d.OtherProfile +
		") 的端点集合不一致 —— " + d.MissingHost + " 缺 " + d.Endpoint + "（" + d.Reason + "）"
}

// schemaDiffs 列出各台与基准台之间的端点集合差异。
//
// 基准台是 routers 数组的第一台。
func (s *Set) schemaDiffs() []schemaDiff {
	if len(s.instances) < 2 {
		return nil
	}
	baseInst := s.instances[0]
	baseProf := baseInst.Client.Profile()
	baseEnabled := baseProf.EnabledEndpoints()
	baseSet := toSet(baseEnabled)

	var out []schemaDiff
	for _, inst := range s.instances[1:] {
		otherProf := inst.Client.Profile()
		got := otherProf.EnabledEndpoints()
		gotSet := toSet(got)

		// 基准台在采、这台不采
		for _, ep := range baseEnabled {
			if gotSet[ep] {
				continue
			}
			reason, kind := diffReason(inst, ep)
			out = append(out, schemaDiff{
				BaseHost: baseInst.Spec.Host, BaseProfile: baseProf.Name(),
				OtherHost: inst.Spec.Host, OtherProfile: otherProf.Name(),
				MissingHost: inst.Spec.Host, Endpoint: ep,
				Reason: reason, Kind: kind,
			})
		}
		// 这台在采、基准台不采
		for _, ep := range got {
			if baseSet[ep] {
				continue
			}
			reason, kind := diffReason(baseInst, ep)
			out = append(out, schemaDiff{
				BaseHost: baseInst.Spec.Host, BaseProfile: baseProf.Name(),
				OtherHost: inst.Spec.Host, OtherProfile: otherProf.Name(),
				MissingHost: baseInst.Spec.Host, Endpoint: ep,
				Reason: reason, Kind: kind,
			})
		}
	}
	return out
}

// diffReason 说明某个端点为什么没被采集，并给出差异的成因分类。
func diffReason(inst *Instance, ep string) (string, string) {
	prof := inst.Client.Profile()
	switch r := prof.DisableReason(ep); r {
	case profile.ReasonConfig:
		return "配置显式排除", kindConfig
	case profile.ReasonNotSupported:
		return "固件不提供该接口", kindFirmware
	case profile.ReasonRepeatedFailure:
		return "连续返回相同错误，已停止采集", kindFault
	case profile.ReasonUnavailable:
		return "已判定不可用", kindFirmware
	case "":
		if !prof.Profile.Supports(ep) {
			return "profile 不支持该端点", kindConfig
		}
		return "原因未知", kindFault
	default:
		return r, kindFirmware
	}
}

// SchemaWarnings 返回**需要人处理**的差异说明：配置不一致或故障导致的缺口。
//
// 这两类才值得 WARN：前者要改配置，后者要修机器。
func (s *Set) SchemaWarnings() []string {
	var out []string
	for _, d := range s.schemaDiffs() {
		if d.Kind == kindFirmware {
			continue
		}
		out = append(out, d.Detail())
	}
	return out
}

// SchemaNotes 返回**固件能力差异**的说明。
//
// 不同型号的路由器固件能力本就不同（实测 Home-GFW 的 R3P 不提供
// newstatus / pppoe_status，Home-RA70 的 RA70 两者都有），
// 这类差异是事实而不是缺陷：写 INFO 说明清楚即可，
// 跨台比较时用 endpoint_supported 过滤掉即可。
//
// 早先这类差异也是 WARN，还建议「统一 profile」——而异构机型下
// 集合完全一致根本做不到，强行统一的唯一办法是丢掉真实存在的数据。
func (s *Set) SchemaNotes() []string {
	var out []string
	for _, d := range s.schemaDiffs() {
		if d.Kind != kindFirmware {
			continue
		}
		out = append(out, d.Detail())
	}
	return out
}

// schemaDiffFor 返回某台与基准台之间的差异说明（供 /routers 展示）。
func (s *Set) schemaDiffFor(host string) []string {
	var out []string
	for _, d := range s.schemaDiffs() {
		if d.MissingHost != host && d.OtherHost != host {
			continue
		}
		out = append(out, d.Detail())
	}
	return out
}

// schemaAligned 判断某台的端点集合是否**不比基准台少**。
//
// 语义（1.19.3 起调整）：只看「少了」，不看「多了」。
//
//   - 比基准台多采几个端点 → 能力差异，不是问题
//   - 比基准台少采几个端点 → 这台的指标集合缺东西，跨台对比会失真
//
// 为什么不再要求「完全一致」：不同型号的路由器固件能力本就不同
// （实测 Home-GFW 的 R3P 不提供 newstatus/pppoe_status，
// Home-RA70 的 RA70 两者都有），集合完全一致在这个场景下根本做不到，
// 而强行「统一」的唯一办法是把真实存在的数据也排掉——
// 连在线终端数 wifi_sta_count 一起丢掉。
//
// 早先的语义（集合完全相同）会让 schemaAligned 在异构机型下恒为 false，
// 于是「两台指标口径不同」被当成缺陷反复提示，而它其实是事实。
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
			Host:           inst.Spec.Host,
			IP:             inst.Spec.IP,
			Profile:        inst.Client.Profile().Name(),
			Mode:           inst.Source.Mode(),
			Ready:          true,
			Endpoints:      inst.Client.Profile().EnabledEndpoints(),
			Disabled:       inst.Client.Profile().DisabledEndpoints(),
			Timeouts:       inst.Client.Profile().TotalTimeouts(),
			EndpointErrors: inst.Client.LastErrors(),
			SchemaAligned:  s.schemaAligned(inst.Spec.Host),
			SchemaDiff:     s.schemaDiffFor(inst.Spec.Host),
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
