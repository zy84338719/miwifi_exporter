// Package profile 收拢「随路由器型号/固件版本而变」的全部差异点。
//
// 这个包存在的唯一理由：把版本差异从采集逻辑里摘出来，让后续遇到新固件时
// 只改这里的一个文件，而不是去翻 client.go / auth.go / collector.go 里
// 到处埋着的 if。
//
// 目前收敛了四类差异：
//
//	端点可用性    哪些 /api/xqnetwork/* 在这台机器上真实存在
//	登录口令散列  newEncryptMode → SHA1 / SHA256 / …
//	登录页解析    登录首页 HTML 的结构（最不稳定的一环）
//	频段映射      wifiIndex 数字 → 频段名
//
// 新增一个固件版本的步骤：
//  1. 在 builtin.go 里加一个 Profile（或在 custom.go 里注册自定义的）
//  2. 登录页结构变了 → 在 login.go 里加一个 LoginPageParser
//  3. 不确定端点是否存在时，交给 detect.go 的自适应探测
//
// 其余代码（router / collector / server）只依赖本包的接口，不需要知道
// 任何具体型号的存在。
package profile

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// 端点名常量。router 与 collector 共享这份定义，避免两处各写一份字符串。
const (
	// EPStatus 对应 /api/misystem/status
	EPStatus = "status"
	// EPDeviceList 对应 /api/misystem/devicelist（v1，ip 为数组）
	EPDeviceList = "devicelist"
	// EPNewStatus 对应 /api/misystem/newstatus（频段在线数 + Mesh 节点数）
	EPNewStatus = "newstatus"
	// EPDeviceListV2 对应 /api/misystem/device_list（v2，ip 为字符串）
	EPDeviceListV2 = "device_list"
	// EPWanInfo 对应 /api/xqnetwork/wan_info
	EPWanInfo = "wan_info"
	// EPWifiDetailAll 对应 /api/xqnetwork/wifi_detail_all
	EPWifiDetailAll = "wifi_detail_all"
	// EPPppoeStatus 对应 /api/xqnetwork/pppoe_status
	EPPppoeStatus = "pppoe_status"
	// EPLanInfo 对应 /api/xqnetwork/lan_info
	EPLanInfo = "lan_info"
	// EPWifiConnDevices 对应 /api/xqnetwork/wifi_connect_devices
	EPWifiConnDevices = "wifi_connect_devices"
	// EPWifiList 对应 /api/xqnetwork/wifi_list
	EPWifiList = "wifi_list"
)

// AllEndpoints 是全部已知端点，顺序即默认采集顺序（越靠前越核心）。
var AllEndpoints = []string{
	EPStatus,
	EPDeviceList,
	EPNewStatus,
	EPDeviceListV2,
	EPWanInfo,
	EPWifiDetailAll,
	EPPppoeStatus,
	EPLanInfo,
	EPWifiConnDevices,
	EPWifiList,
}

// Profile 描述一类固件的行为差异。
//
// 实现必须是不可变的：能力状态（自适应探测的结果）由 Instance 单独承载，
// 避免全局可变的 profile 在多实例下互相污染。
type Profile interface {
	// Name 是 profile 标识，用于配置项 ROUTER_PROFILE。
	Name() string
	// Description 是一句话说明它覆盖哪一类固件。
	Description() string
	// Endpoints 列出该 profile 会主动请求的端点。
	Endpoints() []string
	// Supports 报告端点是否在预期之内。
	Supports(endpoint string) bool
	// LoginPageParser 返回该固件登录首页的解析器。
	LoginPageParser() LoginPageParser
	// Hasher 按固件上报的 newEncryptMode 返回口令散列函数。
	//
	// 参数是 init_info 里的 newEncryptMode，取值由固件自己给出（不是猜的），
	// 但「哪个取值对应哪种算法」是版本差异，所以放在 profile 里。
	Hasher(mode int) (PasswordHasher, error)
	// BandFor 把 wifi_connect_devices 里的 wifiIndex 映射成频段名。
	//
	// 不同型号的射频数量不同：老机器只有 wifiIndex 1/2，
	// 新机器才有 3（5GHz-high，即 5GHz 3rd radio）。
	BandFor(wifiIndex int) string
	// TimeoutFor 返回该端点需要的专用超时；返回 false 表示沿用全局值。
	//
	// 存在的理由来自真机实测：/api/xqnetwork/wifi_list 会触发一次 WiFi 扫描，
	// 在两台不同型号的小米路由器上分别耗时约 13.4s / 14s，
	// 而其余端点都在 0.2–0.5s 内返回。用同一个 5s 全局超时的话，
	// 这个端点每轮都必然失败——不是它坏了，是超时给短了。
	TimeoutFor(endpoint string) (time.Duration, bool)
}

// PasswordHasher 计算最终提交给路由器的口令散列。
type PasswordHasher func(password, key, nonce string) string

// LoginPageInfo 是从登录首页解析出的登录要素。
type LoginPageInfo struct {
	// Key 是页面里的加密 key。
	Key string
	// DeviceID 参与 nonce 计算。
	DeviceID string
}

// LoginPageParser 从登录首页 HTML 中提取 key 与 deviceId。
type LoginPageParser interface {
	Parse(html string) (LoginPageInfo, error)
	// Describe 便于日志里说清当前用的是哪套解析规则。
	Describe() string
}

// Instance 是一个 Profile 的运行期实例，额外承载「自适应探测」的状态。
//
// 为什么不让 Profile 自己可变：profile 会被注册到全局 Registry（多个
// Client 共用同一份定义），而「这台机器上的 wan_info 到底存不存在」是
// 每台机器一份的运行时事实。
type Instance struct {
	Profile Profile

	mu sync.RWMutex
	// disabled 记录被探测为「本机不存在 / 不可用」的端点。
	disabled map[string]bool
	// disabledAt 记录判定时间，用于定期复探（固件升级后接口可能回来）。
	disabledAt map[string]int64
	// pinned 记录**由配置显式排除**的端点。
	//
	// 它们与「探测判定为不存在」必须分开：探测结论会随固件变化而复探，
	// 而配置是用户的明确意志，不该被任何自动机制推翻。真机教训：R3P 的
	// pppoe_status 在 compose 里写了 excludeEndpoints，启动日志也确实打了
	// 「按配置排除端点」，结果 30 分钟后的一次复探把它放回了探测名单，
	// 从此每轮 scrape 都 partial——配置形同虚设且毫无提示。
	pinned map[string]bool
	// timeouts 记录端点连续超时次数，totalTimeouts 记录累计超时次数。
	// 两者**仅用于观测**，不参与停问决策——理由见 RecordResult。
	timeouts      map[string]int
	totalTimeouts int64
	// repeated 记录每个端点「连续收到同一个错误」的次数与该错误文本。
	// 参与停问决策，但判据是错误文本完全相同，见 NoteRepeatedFailure。
	repeated map[string]repeatedFailure
	// timeoutThreshold 是连续超时多少次后判定该端点不可用。
	timeoutThreshold int
	// now 可注入，便于测试。
	now func() int64
}

// repeatedFailure 是一次「连续重复的失败」的计数。
type repeatedFailure struct {
	// text 是错误文本（已截断），用作「是不是同一个错误」的比较键。
	text string
	// run 是连续相同的次数；一旦 text 变化就归 1。
	run int
}

// DefaultTimeoutThreshold 是连续超时的默认降级阈值。
//
// 取 3 而不是 1：单次超时可能是路由器忙（刷 WiFi 列表、正在组播），
// 立刻停问会误伤。三次连续超时基本可以判定这个接口在这台机器上不可用。
const DefaultTimeoutThreshold = 3

// RepeatedFailureThreshold 是「连续多少次收到完全相同的非超时错误」后
// 停问该端点。
//
// 取 5 而不是 1 或 2：路由器重启、固件升级、并发打满都可能让某个端点连着
// 失败几轮，阈值太低会把健康端点误杀。而 5 轮（poll 模式下约 2.5 分钟，
// pull 模式下取决于抓取间隔）已经足够长到能区分「抖动」与「压根没有」。
//
// 这个阈值只作用于**错误文本完全相同**的连续失败。文本里带着耗时、端口、
// 熔断状态这类每次都变的成分时，比较键自然对不上，连续计数不会累积。
const RepeatedFailureThreshold = 5

// repeatedFailureKeyMax 是错误文本比较键的长度上限。
//
// 只为防止某个固件把整页 HTML 或超长 JSON 塞进错误消息里，把 map 的键
// 撑成一个几百 KB 的字符串。截断后仍然足以区分「同一个错误」。
const repeatedFailureKeyMax = 200

// noteKey 归一化错误文本，用作「是不是同一个错误」的比较键。
func noteKey(text string) string {
	if len(text) > repeatedFailureKeyMax {
		return text[:repeatedFailureKeyMax]
	}
	return text
}

// NewInstance 用给定的 profile 构造实例。
func NewInstance(p Profile) *Instance {
	return NewInstanceWithTimeoutThreshold(p, DefaultTimeoutThreshold)
}

// NewInstanceWithTimeoutThreshold 构造实例并指定连续超时降级阈值。
// threshold <= 0 时回落到 DefaultTimeoutThreshold。
func NewInstanceWithTimeoutThreshold(p Profile, threshold int) *Instance {
	if threshold <= 0 {
		threshold = DefaultTimeoutThreshold
	}
	return &Instance{
		Profile:          p,
		disabled:         make(map[string]bool),
		disabledAt:       make(map[string]int64),
		pinned:           make(map[string]bool),
		timeouts:         make(map[string]int),
		repeated:         make(map[string]repeatedFailure),
		timeoutThreshold: threshold,
		now:              unixNow,
	}
}

// Name 返回 profile 名。
func (i *Instance) Name() string { return i.Profile.Name() }

// EnabledEndpoints 返回本轮实际会请求的端点。
func (i *Instance) EnabledEndpoints() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	out := make([]string, 0, len(i.Profile.Endpoints()))
	for _, ep := range i.Profile.Endpoints() {
		if i.disabled[ep] {
			continue
		}
		out = append(out, ep)
	}
	return out
}

// Enabled 报告端点本轮是否会被请求。
func (i *Instance) Enabled(endpoint string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return !i.disabled[endpoint] && i.Profile.Supports(endpoint)
}

// DisabledEndpoints 返回已被判定为不可用的端点（已排序）。
func (i *Instance) DisabledEndpoints() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	out := make([]string, 0, len(i.disabled))
	for ep, off := range i.disabled {
		if off {
			out = append(out, ep)
		}
	}
	sort.Strings(out)
	return out
}

// Disable 把端点标记为「本机不存在」，之后不再请求。
//
// 这是**探测结论**，会被周期性复探推翻——固件升级后接口可能回来。
// 配置显式排除请用 Pin。
func (i *Instance) Disable(endpoint string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.disabled[endpoint] {
		return
	}
	i.disabled[endpoint] = true
	i.disabledAt[endpoint] = i.now()
}

// Pin 把端点永久钉在「不采集」状态，**不参与周期性复探**。
//
// 用于用户通过 EXCLUDE_ENDPOINTS / routers[].excludeEndpoints 显式排除的端点。
//
// 为什么不复用 Disable：探测结论的前提是「这台机器上暂时没有这个接口」，
// 所以要留复探口子；而配置的前提是「我确定不要这个接口」，复探只会把用户的
// 明确配置推翻。真机上就是这么翻车的——R3P 的 pppoe_status 在 compose 里
// 写了 excludeEndpoints，启动日志也打了「按配置排除端点」，30 分钟后的一次
// 复探把它放回探测名单，此后每轮 scrape 都 partial，而配置里那行字从头到尾
// 看起来一直在生效。
//
// 钉死之后仍可通过 Enable 解除（重启进程即可），代价只是固件升级后需要重启。
func (i *Instance) Pin(endpoint string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.disabled[endpoint] = true
	i.disabledAt[endpoint] = i.now()
	i.pinned[endpoint] = true
}

// Pinned 返回当前被配置显式排除的端点（已排序）。
func (i *Instance) Pinned() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	var out []string
	for ep, on := range i.pinned {
		if on {
			out = append(out, ep)
		}
	}
	sort.Strings(out)
	return out
}

// Enable 恢复一个被禁用的端点，并解除它的「钉死」状态。
func (i *Instance) Enable(endpoint string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.disabled, endpoint)
	delete(i.disabledAt, endpoint)
	delete(i.pinned, endpoint)
}

// RetryDisabled 清理掉判定时间超过 ttl 的禁用记录，用于周期性复探。
//
// 没有这一步的话，一台路由器固件升级后新长出来的接口就永远采不到了——
// 「自适应降级」很容易变成「自适应遗忘」。复探通过 Enable 走同一条路径。
//
// **钉死的端点（Pin）一律跳过**：复探的前提是「当初的结论可能已经过时」，
// 而配置排除不是结论，是用户的明确意志。
func (i *Instance) RetryDisabled(ttlMillis int64) []string {
	if ttlMillis <= 0 {
		return nil
	}
	now := i.now()

	i.mu.Lock()
	defer i.mu.Unlock()

	var revived []string
	for ep := range i.disabled {
		if i.pinned[ep] {
			continue
		}
		if now-i.disabledAt[ep] >= ttlMillis {
			delete(i.disabled, ep)
			delete(i.disabledAt, ep)
			delete(i.timeouts, ep)
			delete(i.repeated, ep)
			revived = append(revived, ep)
		}
	}
	sort.Strings(revived)
	return revived
}

// RecordResult 记录一次采集结果，维护连续/累计超时计数。
//
// **它不会导致端点被停问**——超时无法区分「这个端点坏了」与
// 「路由器被另一个慢端点堵住了」。真机实测：wifi_list 触发 WiFi 扫描时
// 会阻塞其余全部 LuCI 请求，连带让九个健康端点超时；按超时停问会把
// 它们全部误杀。停问只认 404/501 这类确定性信号（见 Disable 的调用方）。
// RecordResult 记录一次**失败**的采集结果，维护连续/累计超时计数。
//
// 参数 timedOut 只回答「这一次是不是超时」。传 false 不代表本轮成功——
// 成功走的是 RecordSuccess。这里刻意不碰 repeated 计数：失败分支里刚由
// NoteRepeatedFailure 累加的连续失败次数，必须活到下一轮才能累积，
// 若在同一次失败里被清零，降级逻辑就永远不会触发。
//
// **超时不会导致端点被停问**——超时无法区分「这个端点坏了」与
// 「路由器被另一个慢端点堵住了」。真机实测：wifi_list 触发 WiFi 扫描时
// 会阻塞其余全部 LuCI 请求，连带让九个健康端点超时；按超时停问会把
// 它们全部误杀。停问只认 404/501 与重复失败这两种确定性信号。
func (i *Instance) RecordResult(endpoint string, timedOut bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if !timedOut {
		delete(i.timeouts, endpoint)
		return
	}
	i.timeouts[endpoint]++
	i.totalTimeouts++
}

// RecordSuccess 记录一次成功采集，清空该端点的所有连续失败计数。
//
// 断续的失败不构成「这台固件压根没有这个接口」的证据——中间成功过就说明
// 接口还在，只是那几轮路由器自己有状况。
func (i *Instance) RecordSuccess(endpoint string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	delete(i.timeouts, endpoint)
	delete(i.repeated, endpoint)
}

// NoteRepeatedFailure 记录一次**非超时**的端点失败，返回是否应当停问该端点。
//
// 为什么需要它：停问原本只认 HTTP 404/501 这一种确定性信号，于是出现了一
// 类「明显失败却永远停不下来」的端点——实测 R3P 上 pppoe_status 连着报
// 错，自适应探测永远不收敛，结果 scrape_total{result="partial"} 只增不减，
// 一次干净的 success 都不会出现，而 endpoint_supported 一直说 1。
//
// 为什么判据是「错误文本完全相同」而不是「失败次数」：这条路径不能重蹈
// 超时降级的覆辙。超时分不清「这个端点坏了」和「路由器被另一个慢端点
// 堵住了」，实测会把九个健康端点连带误杀。而同一个端点连续 5 轮吐出
// **逐字相同**的错误，基本不存在巧合——最典型的就是「这台固件没有这个
// 接口」，LuCI 对不存在的路径返回的正是固定的 404 页面。
//
// 停问后仍会按 RecheckDue 复探，所以固件升级后新长出来的接口不会被永久
// 忘掉；真要立刻停问，用户仍可以用 ExcludeEndpoints 显式排除。
func (i *Instance) NoteRepeatedFailure(endpoint, errText string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()

	key := noteKey(errText)
	prev := i.repeated[endpoint]
	if prev.text != key {
		prev = repeatedFailure{text: key}
	}
	prev.run++
	i.repeated[endpoint] = prev

	return prev.run >= RepeatedFailureThreshold
}

// RepeatedFailuresOf 返回端点当前连续相同错误的次数（仅供观测与日志）。
func (i *Instance) RepeatedFailuresOf(endpoint string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.repeated[endpoint].run
}

// TotalTimeouts 返回进程内累计的超时次数。
func (i *Instance) TotalTimeouts() int64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.totalTimeouts
}

// ConsecutiveTimeoutsOf 返回当前连续超时次数达到阈值的端点（仅供观测）。
func (i *Instance) ConsecutiveTimeoutsOf() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	var out []string
	for ep, n := range i.timeouts {
		if n >= i.timeoutThreshold {
			out = append(out, ep)
		}
	}
	sort.Strings(out)
	return out
}

// ConsecutiveTimeouts 返回端点当前的连续超时次数（供指标/日志观测）。
func (i *Instance) ConsecutiveTimeouts(endpoint string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.timeouts[endpoint]
}

// TimeoutThreshold 返回连续超时降级阈值。
func (i *Instance) TimeoutThreshold() int { return i.timeoutThreshold }

// registry 是全局 profile 注册表。
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Profile)
)

// Register 注册一个 profile。同名会被拒绝——静默覆盖会让「显式指定
// ROUTER_PROFILE 到底选中了谁」变得无法排查。
func Register(p Profile) error {
	if p == nil || p.Name() == "" {
		return fmt.Errorf("profile: 注册的 profile 必须有非空 Name")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[p.Name()]; dup {
		return fmt.Errorf("profile: %q 已注册", p.Name())
	}
	registry[p.Name()] = p
	return nil
}

// Lookup 按名字取 profile。
func Lookup(name string) (Profile, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	return p, ok
}

// Names 返回全部已注册 profile 名（已排序）。
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Resolve 按名字取 profile；未注册时返回带候选清单的错误。
func Resolve(name string) (Profile, error) {
	if p, ok := Lookup(name); ok {
		return p, nil
	}
	return nil, fmt.Errorf("profile: 未知 profile %q（可用: %v）", name, Names())
}
