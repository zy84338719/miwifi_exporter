// Package collector 把路由器快照翻译成 Prometheus 指标。
package collector

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oekit/concurrency/singleflight"
	"github.com/oekit/logx"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/router"
)

// Source 是一条采集链路：pull 模式下同步取数，poll 模式下读缓存快照。
type Source interface {
	// Collect 返回一份快照。任何情况下都不返回 nil。
	Collect(ctx context.Context) *router.Snapshot
	// Mode 返回 "pull" 或 "poll"，用于自监控指标的 mode 标签。
	Mode() string
}

// CollectorOptions 是 Collector 的构造参数。
type CollectorOptions struct {
	// Namespace 是指标名前缀，默认 "miwifi"。
	Namespace string
	// Host 是 host 标签的取值，来自 ROUTER_HOST。
	Host string
	// Source 是采集链路。
	Source Source
	// Profile 是固件 profile，决定频段映射等版本相关行为；nil 表示用默认三频段映射。
	Profile *profile.Instance
	// Descs 允许多实例共享同一份指标描述符。
	//
	// prometheus.Registry 按 Desc 内容判重，多台路由器各建各的 Collector 会
	// 被判成重复收集器。多机场景下由 Set 建一份共享给所有实例。
	Descs *descs
	// Version 写入 build_info。
	Version string
	// Logger 覆盖默认 logger。
	Logger logx.Logger
}

// Collector 实现 prometheus.Collector，输出路由器的全部指标。
type Collector struct {
	d      *descs
	build  *buildInfo
	host   string
	source Source
	self   *selfMetrics
	logger logx.Logger
	// prof 提供与固件版本相关的映射（当前只有 wifiIndex → 频段名）。
	prof *profile.Instance

	// flight 折叠并发 scrape：Prometheus 并不保证同一 target 不会并发拉取，
	// 两个并发 scrape 会各自去打十个路由器接口，对一台家用路由器是实打实的
	// 负担。折叠后同时刻只会有一次真实采集，第二个请求直接复用结果。
	flight singleflight.Flight[*router.Snapshot]

	// reqCtx 承载当前抓取请求的 context，由 HTTP 层在 Gather 前注入。
	//
	// 为什么需要它：prometheus.Collector.Collect 的签名里没有 ctx，
	// 于是采集只能从 context.Background() 起算。后果是 Prometheus 到了
	// scrape_timeout 直接断开之后，exporter 仍然会把十个接口老老实实跑完
	// （路由器慢的时候还会拖上好几十秒）——没人要的结果，白白压路由器。
	reqCtx atomic.Pointer[requestCtx]
}

// requestCtx 是个包装类型，避免在 atomic.Pointer 里存 context.Context 接口值
// 造成「接口内含不可比较的具体类型」的隐患。
type requestCtx struct {
	ctx context.Context
}

// SetRequestContext 供 HTTP 层在每次抓取前注入请求 context，取用后即失效。
//
// 用取用即失效而不是长期保存，是为了让下一次 Collect 一定能拿到新鲜值：
// 万一这次没走到 Collect（比如路由注册有变动），也不会拿着上一次请求的
// 已取消 ctx 去做事。
func (c *Collector) SetRequestContext(ctx context.Context) {
	c.reqCtx.Store(&requestCtx{ctx: ctx})
}

// takeRequestContext 取一次请求 ctx，取后清空。
func (c *Collector) takeRequestContext() context.Context {
	rc := c.reqCtx.Swap(nil)
	if rc == nil {
		return context.Background()
	}
	// 兜底：万一存进来的是上一次遗留的、已经取消的 ctx，
	// 与其拿一个死 ctx 去采集，不如退回 Background。
	if rc.ctx.Err() != nil {
		return context.Background()
	}
	return rc.ctx
}

// New 构造 Collector。
func New(opts CollectorOptions) (*Collector, error) {
	ns := opts.Namespace
	if ns == "" {
		ns = "miwifi"
	}
	if opts.Source == nil {
		return nil, fmt.Errorf("collector: Source 不能为空")
	}

	logger := opts.Logger
	if logger == nil {
		logger = logx.Delegating("collector")
	}

	prof := opts.Profile
	if prof == nil {
		prof, _ = profile.Select(profile.Default)
	}

	d := opts.Descs
	if d == nil {
		d = newDescs(ns)
	}

	return &Collector{
		d:      d,
		build:  newBuildInfo(ns, opts.Version),
		host:   opts.Host,
		source: opts.Source,
		prof:   prof,
		self: newSelfMetrics(selfMetricOptions{
			Namespace: ns, Mode: opts.Source.Mode(), Version: opts.Version,
			Prof: prof, Host: opts.Host, Logger: logger,
		}),
		logger: logger,
	}, nil
}

// Self 返回自监控指标，供单独注册到 registry。
func (c *Collector) Self() prometheus.Collector { return c.self }

// SnapshotFor 直接取一份快照并更新自监控指标。
//
// Collect 内部走的是同一套逻辑，但那条路径被 prometheus.Collector 的签名
// 限死（没有 ctx 参数）。暴露这个方法有两个用处：一是测试可以在不经过
// 指标编码的情况下断言采集行为，二是给将来「预热 / 主动刷新」留个入口。
// 注意它**不会**消费注入的请求 ctx——那是 Collect 专属的语义。
func (c *Collector) SnapshotFor(ctx context.Context) (*router.Snapshot, error) {
	start := time.Now()
	// DoShared（concurrency v0.12 新增，见 oekit/concurrency#18）比 Do 多一个
	// shared 标志，用来区分「这一轮真的去采了」和「复用了并发中的那一轮」。
	// Prometheus 并不保证同一 target 不会并发拉取，折叠确实会发生；
	// 没有这个标志就只能靠耗时猜测折叠有没有生效。
	snap, shared, err := c.flight.DoShared(ctx, "scrape", func(cctx context.Context) (*router.Snapshot, error) {
		return c.source.Collect(cctx), nil
	})
	if shared {
		c.logger.Debug("复用了并发中的采集结果", "host", c.host, "mode", c.source.Mode())
	}
	c.self.Observe(c.source.Mode(), time.Since(start), snap)
	return snap, err
}

// Describe 实现 prometheus.Collector。
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.d.all() {
		ch <- d
	}
	c.build.Describe(ch)
}

// Collect 实现 prometheus.Collector。
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx := c.takeRequestContext()
	start := time.Now()

	snap, err := c.flight.Do(ctx, "scrape", func(cctx context.Context) (*router.Snapshot, error) {
		return c.source.Collect(cctx), nil
	})

	// 客户端先走了一步（Prometheus 超时断开）不算路由器的故障，
	// 因此这一轮不写自监控指标——否则 router_up 会被一个「没人要的结果」
	// 打成 0，告警随之误报。指标保持上一次的值，即「最后一次已知状态」。
	if ctx.Err() == nil {
		c.self.Observe(c.source.Mode(), time.Since(start), snap)
	} else {
		c.logger.Debug("抓取被客户端取消，不计入自监控指标", "err", ctx.Err().Error())
	}

	if err != nil && ctx.Err() == nil {
		c.logger.Error("采集失败", "err", err.Error())
	}
	if snap == nil {
		return
	}

	c.build.Collect(ch)
	c.Emit(ch, snap)
}

// MarkUnreachable 在采集异常中止时把该路由器明确标成不可达。
//
// 「没采到」和「采到了但是零」在监控上完全不是一回事：后者会被误读成
// 「路由器各项指标都是 0」。这里只动 router_up，让告警能区分两者。
func (c *Collector) MarkUnreachable() {
	c.self.MarkUnreachable()
}

// Emit 把快照编码成指标。多实例场景由 Set 调用。
func (c *Collector) Emit(ch chan<- prometheus.Metric, snap *router.Snapshot) {
	c.emit(ch, snap)
}

// emit 把快照翻译成指标。分成若干组，每组一个函数——旧版把 180 行全都塞在
// 一个 Collect 里，任何一处改动都要通读全文才能确认没碰到别的指标。
func (c *Collector) emit(ch chan<- prometheus.Metric, snap *router.Snapshot) {
	c.emitSystem(ch, snap)
	c.emitWAN(ch, snap)
	c.emitDevices(ch, snap)
	c.emitWiFi(ch, snap)
}

// emitSystem 输出 CPU / 内存 / 终端计数 / 运行时长 / 硬件标识。
func (c *Collector) emitSystem(ch chan<- prometheus.Metric, s *router.Snapshot) {
	st := &s.Status
	host := c.host

	memTotal := memoryTotalMB(st)
	memUsageMB := st.Mem.Usage * memTotal

	emit(ch, c.d.cpucores, float64(st.CPU.Core), host)
	emit(ch, c.d.cpumhz, cpuMHz(st), host)
	emit(ch, c.d.cpuload, st.CPU.Load, host)

	emit(ch, c.d.memoryTotalMB, memTotal, host)
	emit(ch, c.d.memoryUsageMB, memUsageMB, host)
	emit(ch, c.d.memoryUsage, st.Mem.Usage, host)

	emit(ch, c.d.countAll, float64(st.Count.All), host)
	emit(ch, c.d.countOnline, float64(st.Count.Online), host)
	emit(ch, c.d.countAllWithoutMash, float64(st.Count.AllWithoutMash), host)
	emit(ch, c.d.countOnlineWithoutMash, float64(st.Count.OnlineWithoutMash), host)

	emit(ch, c.d.uptime, parseFloat(st.UpTime), host)

	emit(ch, c.d.platform, 1, host, st.Hardware.Platform)
	emit(ch, c.d.version, 1, host, st.Hardware.Version)
	emit(ch, c.d.sn, 1, host, st.Hardware.Sn)
	emit(ch, c.d.mac, 1, host, st.Hardware.Mac)
}

// emitWAN 输出 WAN 速率、流量、地址与 PPPoE/LAN 状态。
func (c *Collector) emitWAN(ch chan<- prometheus.Metric, s *router.Snapshot) {
	host := c.host
	wan := &s.Status.Wan

	emit(ch, c.d.wanUploadSpeed, parseFloat(wan.UpSpeed), host)
	emit(ch, c.d.wanDownloadSpeed, parseFloat(wan.DownSpeed), host)
	emit(ch, c.d.wanUploadTraffic, parseFloat(wan.Upload), host)
	emit(ch, c.d.wanDownTraffic, parseFloat(wan.Download), host)

	for _, ipv4 := range s.WanInfo.Ipv4 {
		emit(ch, c.d.ipv4, 1, host, ipv4.IP)
		mask, err := subnetMaskToLen(ipv4.Mask)
		if err != nil {
			c.logger.Warn("解析子网掩码失败", "host", host, "mask", ipv4.Mask, "err", err.Error())
		}
		emit(ch, c.d.ipv4Mask, float64(mask), host, ipv4.IP)
	}
	for _, ipv6 := range s.WanInfo.Ipv6Info.IP6Addr {
		emit(ch, c.d.ipv6, 1, host, ipv6)
	}

	emit(ch, c.d.wanLinkStatus, float64(s.PppoeStatus.Status), host)
	// 协商速率解析不出来时发 0，而不是不发这一条。
	//
	// 两种行为在单台场景下都讲得通，但多台场景下必须选一个：不发的话，
	// 同一台 exporter 里「上报了协商速率的路由器」有这条序列、「没上报的」
	// 完全没有，跨路由器对比时会出现一条有、一条没有的空洞。
	// 发 0 至少保证指标集合一致，语义写在 HELP 里：0 = 路由器未上报。
	speed, err := strconv.ParseFloat(s.PppoeStatus.WanSpeed, 64)
	if err != nil {
		speed = 0
	}
	emit(ch, c.d.wanNegSpeed, speed, host)
	emit(ch, c.d.wanInfo, 1, host, s.PppoeStatus.IP.Address, s.PppoeStatus.GW, strings.Join(s.PppoeStatus.DNS, ","))

	for i, link := range s.LanInfo.LinkList {
		emit(ch, c.d.lanPortLink, float64(link), host, strconv.Itoa(i))
	}
	emit(ch, c.d.lanUptime, float64(s.LanInfo.Info.Uptime), host)
}

// emitDevices 输出终端流量、速率、在线时长与设备详情。
func (c *Collector) emitDevices(ch chan<- prometheus.Metric, s *router.Snapshot) {
	host := c.host

	// status.dev 里的终端只有 mac，没有 IP/isAP；需要用 devicelist 补齐。
	// 建一张索引把关联成本从 O(n*m) 降到 O(n+m)——设备多的家里几十台终端
	// 每次 scrape 做几千次比较纯属浪费。
	ipIndex := make(map[string]struct {
		ip   string
		isAP string
	}, len(s.DeviceList.List))
	for _, d := range s.DeviceList.List {
		if len(d.IP) == 0 {
			continue
		}
		ipIndex[d.Mac] = struct {
			ip   string
			isAP string
		}{ip: d.IP[0].IP, isAP: strconv.Itoa(d.IsAP)}
	}

	for _, dev := range s.Status.Dev {
		var devIP, devIsAP string
		if v, ok := ipIndex[dev.Mac]; ok {
			devIP, devIsAP = v.ip, v.isAP
		}
		emit(ch, c.d.deviceUploadTraffic, toFloat(dev.Upload), host, devIP, dev.Mac, dev.DevName, devIsAP)
		emit(ch, c.d.deviceDownTraffic, toFloat(dev.Download), host, devIP, dev.Mac, dev.DevName, devIsAP)
	}

	for _, dev := range s.DeviceList.List {
		if len(dev.IP) == 0 {
			continue
		}
		labels := []string{host, dev.IP[0].IP, dev.Mac, dev.Name, strconv.Itoa(dev.IsAP)}
		emit(ch, c.d.deviceUploadSpeed, parseFloat(dev.Statistics.UpSpeed), labels...)
		emit(ch, c.d.deviceDownSpeed, parseFloat(dev.Statistics.DownSpeed), labels...)
		emit(ch, c.d.deviceOnlineTime, parseFloat(dev.Statistics.Online), labels...)
	}

	for _, dev := range s.DeviceListV2.Devices {
		name := dev.Name
		if name == "" {
			name = dev.OName
		}
		labels := []string{host, dev.IP, dev.Mac, name}
		emit(ch, c.d.deviceWifiQuality, float64(dev.WifiQuality), labels...)
		emit(ch, c.d.deviceConnectionType, 1, append(append([]string{}, labels...), strconv.Itoa(dev.ConnectionType))...)
		emit(ch, c.d.devicePControl, float64(dev.PControl.Enabled), labels...)
	}
}

// emitWiFi 输出射频详情、频段在线数、Mesh 节点数与周围 WiFi 环境。
func (c *Collector) emitWiFi(ch chan<- prometheus.Metric, s *router.Snapshot) {
	host := c.host

	for _, info := range s.WifiDetailAll.Info {
		emit(ch, c.d.wifiDetail, parseFloat(info.Status),
			host, info.Ssid, info.Status, joinBandList(info.ChannelInfo.BandList),
			fmt.Sprintf("%v", info.ChannelInfo.Channel))
	}

	emit(ch, c.d.wifiStaCount, float64(s.NewStatus.G2.OnlineStaCount), host, "2.4GHz")
	emit(ch, c.d.wifiStaCount, float64(s.NewStatus.G5.OnlineStaCount), host, "5GHz")
	emit(ch, c.d.wifiStaCount, float64(s.NewStatus.G5H.OnlineStaCount), host, "5GHz-high")
	emit(ch, c.d.meshCapCount, float64(s.NewStatus.CapCount), host)
	emit(ch, c.d.meshReCount, float64(s.NewStatus.ReCount), host)

	for _, d := range s.WifiConnDevs.List {
		emit(ch, c.d.wifiDeviceSig, float64(d.Signal), host, d.Mac, c.bandFor(d.WifiIndex))
	}

	for _, w := range s.WifiList.List {
		emit(ch, c.d.neighborWiFiRSSI, float64(w.RSSI), host, w.SSID, w.BSSID, w.Band, strconv.Itoa(w.Channel))
	}
	emit(ch, c.d.neighborWiFiCnt, float64(len(s.WifiList.List)), host)
}

// emit 发送一个常量指标。
func emit(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labelValues ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labelValues...)
}

// parseFloat 解析路由器的字符串数字，失败返回 0。
//
// 旧实现在解析失败时打一行 "err: " 日志，而 status.dev 这类字段在部分固件上
// 会返回空串或 "-"——每次 scrape 刷十几行无意义日志。这里静默返回 0。
func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// toFloat 处理 status.dev 里的 any 类型字段（不同固件给数字或字符串）。
func toFloat(v any) float64 {
	switch x := v.(type) {
	case string:
		return parseFloat(x)
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	default:
		return 0
	}
}

// cpuMHz 把固件的 "1.2GHz" / "880MHz" 统一换算成 MHz。
func cpuMHz(st *router.Status) float64 {
	switch {
	case strings.HasSuffix(st.CPU.Hz, "GHz"):
		return parseFloat(strings.TrimSuffix(st.CPU.Hz, "GHz")) * 1000
	case strings.HasSuffix(st.CPU.Hz, "MHz"):
		return parseFloat(strings.TrimSuffix(st.CPU.Hz, "MHz"))
	default:
		return 0
	}
}

// memoryTotalMB 把 "256MB" 之类的字面量换算成 MB 数值。
func memoryTotalMB(st *router.Status) float64 {
	return parseFloat(strings.TrimSuffix(st.Mem.Total, "MB"))
}

// joinBandList 把 ["2.4","5"] 拼成 "2.4/5MHz"。
//
// 末段补 MHz 是旧版的行为，看着别扭但属于既有标签取值，改了会让历史面板的
// band_list 标签对不上，所以原样保留。
func joinBandList(bands []string) string {
	var sb strings.Builder
	for i, band := range bands {
		sb.WriteString(band)
		if i != len(bands)-1 {
			sb.WriteString("/")
		} else {
			sb.WriteString("MHz")
		}
	}
	return sb.String()
}

// bandFor 把 wifiIndex 映射为频段名。
//
// 映射规则因型号而异（双频段机器没有 5GHz-high），因此走固件 profile，
// 不再在采集层硬编码。映射表见 internal/profile/builtin.go。
func (c *Collector) bandFor(idx int) string {
	return c.prof.Profile.BandFor(idx)
}

// subnetMaskToLen 把 "255.255.255.0" 转成 CIDR 前缀位数 24。
func subnetMaskToLen(netmask string) (int, error) {
	ipSplit := strings.Split(netmask, ".")
	if len(ipSplit) != 4 {
		return 0, fmt.Errorf("netmask:%v is not valid, pattern should like: 255.255.255.0", netmask)
	}
	var bytes [4]byte
	for i, v := range ipSplit {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("netmaskToInt call strconv.Atoi error:[%v] string value is: [%s]", err, v)
		}
		if n > 255 {
			return 0, fmt.Errorf("netmask cannot greater than 255, current value is: [%s]", v)
		}
		bytes[i] = byte(n)
	}
	ones, _ := net.IPv4Mask(bytes[0], bytes[1], bytes[2], bytes[3]).Size()
	return ones, nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
