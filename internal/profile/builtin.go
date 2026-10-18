package profile

import (
	"fmt"
	"time"
)

// 本文件是**新增固件版本时的主要落点**。
//
// 绝大多数情况下，你只需要在这里加一个 profile：声明它支持哪些端点、
// 用哪个登录页解析器、频段怎么映射。其余包不用改。
//
// 现内置三个：
//
//	default   全部 10 个端点。新旧固件通吃，是兜底选择。
//	minimal   只有 status + devicelist。老到连 newstatus / xqnetwork 都没有的机器。
//	xqnetwork  明确只信任 misystem 系列并跳过 xqnetwork 系列。
//	          供已知 xqnetwork 返回脏数据的固件显式选用。
//
// 之所以内置这么多而不是留一个 auto：auto 是**启发式**的（靠探测端点存不存在），
// 而现场排障时需要的是一个能写进配置、可以复现的选择。

// baseProfile 提供所有 profile 共享的默认行为，
// 具体 profile 只覆盖需要差异的部分。
type baseProfile struct {
	name        string
	description string
	endpoints   []string
	// supported 是 endpoints 的集合形式。
	//
	// 存在它是因为 Supports 在每轮采集里会被每个端点各问一次
	// （Instance.Enabled → Profile.Supports），线性扫描会让这段变成
	// O(n²)。n=10 时当然无所谓，但这是热路径，且改成查表没有任何代价。
	supported map[string]bool
	parser    LoginPageParser
	bands     map[int]string
	// timeouts 是端点 → 专用超时的覆盖表。
	timeouts map[string]time.Duration
}

// timeoutFor 返回端点专用超时，未覆盖时返回 false。
func (p *baseProfile) TimeoutFor(endpoint string) (time.Duration, bool) {
	d, ok := p.timeouts[endpoint]
	return d, ok
}

func (p *baseProfile) Name() string        { return p.name }
func (p *baseProfile) Description() string { return p.description }
func (p *baseProfile) Endpoints() []string { return p.endpoints }

func (p *baseProfile) Supports(endpoint string) bool {
	return p.supported[endpoint]
}

func (p *baseProfile) LoginPageParser() LoginPageParser { return p.parser }

// Hasher 按固件上报的 newEncryptMode 选算法。
//
// newEncryptMode 的取值来自 init_info 的响应，是固件自己说的，不是猜的：
//
//	0 → SHA1（较老的固件）
//	1 → SHA256
//
// 将来若出现新模式，在这里加一条 case 即可，别处不用动。
// 未识别的取值明确返回错误——宁可登录失败让人看见，
// 也不要悄悄用 SHA1 去撞一个 SHA256 的门。
func (p *baseProfile) Hasher(mode int) (PasswordHasher, error) {
	switch mode {
	case 0:
		return sha1Hasher, nil
	case 1:
		return sha256Hasher, nil
	default:
		return nil, fmt.Errorf("profile: 未知的 newEncryptMode=%d（该固件可能需要新增散列算法）", mode)
	}
}

func (p *baseProfile) BandFor(wifiIndex int) string {
	if name, ok := p.bands[wifiIndex]; ok {
		return name
	}
	return "unknown"
}

// standardBands 是三频段机器的映射（绝大多数 AX 系列的形态）。
var standardBands = map[int]string{
	1: "2.4GHz",
	2: "5GHz",
	3: "5GHz-high",
}

// dualBands 是双频段机器的映射。
//
// 关键差异：wifiIndex=3 在双频段机器上不存在，某些固件会把未使用射频
// 报成 3。若沿用三频段映射，这些幽灵终端会被错标成 "5GHz-high"。
var dualBands = map[int]string{
	1: "2.4GHz",
	2: "5GHz",
}

// slowEndpoints 是「天然慢、但慢得有道理」的端点。
//
// 刻意只放经过判断的端点，而不是「实测慢就放进来」。
//
// 反面教材：/api/xqnetwork/wifi_list 实测要 13–14s（会触发一次 WiFi 扫描），
// 曾经被按这个思路配成 20s 专用超时。结果是更糟——扫描期间路由器会阻塞
// 其它所有 LuCI 请求，于是九个本来 0.2s 就返回的端点一起被拖到超时，
// 整轮抓取反而从 5s 变成全面失败。
//
// 真机两台（192.168.31.1 与 10.10.30.248）表现一致，说明这不是型号问题。
// 结论：宁可让它超时后被自适应降级停掉，也不要为了「让它成功」而拖垮整台。
// 需要这个指标的用户用 ENDPOINT_TIMEOUTS=wifi_list=20s 显式打开，并接受代价。
var slowEndpoints = map[string]time.Duration{}

func newBase(name, description string, endpoints []string, bands map[int]string) *baseProfile {
	supported := make(map[string]bool, len(endpoints))
	timeouts := make(map[string]time.Duration)
	for _, ep := range endpoints {
		supported[ep] = true
		if d, ok := slowEndpoints[ep]; ok {
			timeouts[ep] = d
		}
	}
	return &baseProfile{
		name:        name,
		description: description,
		endpoints:   endpoints,
		supported:   supported,
		parser:      DefaultLuCIParser(),
		bands:       bands,
		timeouts:    timeouts,
	}
}

// endpointSet 把端点名列表去重成集合（保持 AllEndpoints 的顺序）。
func endpointSet(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, ep := range names {
		out = append(out, ep)
	}
	return out
}

// withoutEndpoint 返回去掉指定端点后的列表，保持原有顺序。
func withoutEndpoint(all []string, drop string) []string {
	out := make([]string, 0, len(all))
	for _, ep := range all {
		if ep != drop {
			out = append(out, ep)
		}
	}
	return out
}

func init() {
	builtins := []Profile{
		newBase(
			Default,
			"默认：全部 10 个端点，新旧固件通吃；配合自适应探测使用",
			AllEndpoints,
			standardBands,
		),
		newBase(
			Minimal,
			"最简：只采 status 与 devicelist，适用于极老固件",
			endpointSet(EPStatus, EPDeviceList),
			dualBands,
		),
		// no-scan：默认集去掉 wifi_list。真机实测该端点会触发一次 WiFi 扫描
		// （13–14s），且扫描期间路由器阻塞其它全部 LuCI 请求——开着它，
		// 一轮抓取里另外九个端点也跟着被拖超时。抓取延迟敏感的场景用这个。
		newBase(
			NoScan,
			"去掉 wifi_list（避免 WiFi 扫描阻塞整台路由器导致其余端点一起超时）",
			withoutEndpoint(AllEndpoints, EPWifiList),
			standardBands,
		),
		newBase(
			NoXQNetwork,
			"跳过全部 xqnetwork 接口，只用 misystem 系列；适用于 xqnetwork 返回脏数据的固件",
			endpointSet(EPStatus, EPDeviceList, EPNewStatus, EPDeviceListV2),
			standardBands,
		),
	}
	for _, p := range builtins {
		// 内置 profile 重名属于代码错误，直接 panic 而不是让启动失败时才发现。
		if err := Register(p); err != nil {
			panic("profile: 注册内置 profile 失败: " + err.Error())
		}
	}
}

// 内置 profile 名。
const (
	// Default 采全部端点。
	Default = "default"
	// Minimal 只采最核心的两个端点。
	Minimal = "minimal"
	// NoScan 采全部端点但不采 wifi_list（见 no-scan profile 说明）。
	NoScan = "no-scan"
	// NoXQNetwork 跳过 xqnetwork 系列。
	NoXQNetwork = "no-xqnetwork"
	// Auto 表示交给自适应探测自行收敛。
	Auto = "auto"
)
