package router

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/oekit/concurrency/async"
	oerr "github.com/oekit/pkg/errors"
)

// EndpointState 描述单个端点在某一轮采集中的结果。
type EndpointState struct {
	// OK 表示本轮取数成功。
	OK bool
	// Stale 表示本轮取数失败，指标里暴露的是上一次成功的值。
	Stale bool
	// Elapsed 是本轮该端点的耗时。
	Elapsed time.Duration
	// Err 是本轮的失败原因，成功时为 nil。
	Err error
}

// Snapshot 是某一轮采集的完整结果。
//
// 取值失败但存在历史成功值时，字段里放的是上一次成功的值，并由 Endpoint
// 标记为 Stale。这样做的取舍是明确的：Prometheus 里宁可要一个「旧但连续」
// 的时间序列（rate() 不至于断档），也不要一片空洞；同时陈旧与否由
// miwifi_exporter_endpoint_stale 显式暴露，不靠猜。
type Snapshot struct {
	Status        Status
	DeviceList    DeviceList
	NewStatus     NewStatus
	DeviceListV2  DeviceListV2
	WanInfo       Info
	WifiDetailAll WifiDetailAll
	PppoeStatus   PppoeStatus
	LanInfo       LanInfo
	WifiConnDevs  WifiConnectDevices
	WifiList      WifiList

	// CollectedAt 是本轮采集的开始时间。
	CollectedAt time.Time
	// Endpoint 记录每个端点在本轮的状态。
	Endpoint map[string]EndpointState
}

// Healthy 报告本轮是否所有端点都成功。
func (s *Snapshot) Healthy() bool {
	for _, st := range s.Endpoint {
		if !st.OK {
			return false
		}
	}
	return len(s.Endpoint) > 0
}

// FailedEndpoints 返回本轮失败的端点名（已排序，便于稳定输出与测试断言）。
func (s *Snapshot) FailedEndpoints() []string {
	var names []string
	for name, st := range s.Endpoint {
		if !st.OK {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// endpoint 把一个 API 端点与 Snapshot 中的字段绑定在一起。
type endpoint struct {
	name string
	path string
	// fetch 只写自己的那个字段，因此并发执行天然无竞争。
	fetch func(ctx context.Context, s *Snapshot) error
	// save 返回字段地址，用于留档。
	save func(s *Snapshot) any
	// restore 把留档写回字段。
	restore func(s *Snapshot, v any)
}

// enabledEndpoints 按 profile 过滤出本轮真正要请求的端点。
//
// profile 已经表达了两层信息：静态的「这类固件有什么接口」，
// 以及运行期自适应探测出来的「这台机器上实际有什么」。
// 被过滤掉的端点连一次请求都不会发，自然也不会有 WARN 日志和恒零指标。
func (c *Client) enabledEndpoints() []endpoint {
	all := c.allEndpoints()
	out := make([]endpoint, 0, len(all))
	for _, ep := range all {
		if c.profile.Enabled(ep.name) {
			out = append(out, ep)
		}
	}
	return out
}

func (c *Client) allEndpoints() []endpoint {
	return []endpoint{
		{
			name: "status", path: pathStatus,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathStatus, &s.Status) },
			save:  func(s *Snapshot) any { return &s.Status },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*Status); ok {
					s.Status = *p
				}
			},
		},
		{
			name: "devicelist", path: pathDeviceList,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathDeviceList, &s.DeviceList) },
			save:  func(s *Snapshot) any { return &s.DeviceList },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*DeviceList); ok {
					s.DeviceList = *p
				}
			},
		},
		{
			name: "newstatus", path: pathNewStatus,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathNewStatus, &s.NewStatus) },
			save:  func(s *Snapshot) any { return &s.NewStatus },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*NewStatus); ok {
					s.NewStatus = *p
				}
			},
		},
		{
			name: "device_list", path: pathDeviceListV2,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathDeviceListV2, &s.DeviceListV2) },
			save:  func(s *Snapshot) any { return &s.DeviceListV2 },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*DeviceListV2); ok {
					s.DeviceListV2 = *p
				}
			},
		},
		{
			name: "wan_info", path: pathWanInfo,
			fetch: c.fetchWanInfo,
			save:  func(s *Snapshot) any { return &s.WanInfo },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*Info); ok {
					s.WanInfo = *p
				}
			},
		},
		{
			name: "wifi_detail_all", path: pathWifiDetailAll,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathWifiDetailAll, &s.WifiDetailAll) },
			save:  func(s *Snapshot) any { return &s.WifiDetailAll },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*WifiDetailAll); ok {
					s.WifiDetailAll = *p
				}
			},
		},
		{
			name: "pppoe_status", path: pathPppoeStatus,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathPppoeStatus, &s.PppoeStatus) },
			save:  func(s *Snapshot) any { return &s.PppoeStatus },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*PppoeStatus); ok {
					s.PppoeStatus = *p
				}
			},
		},
		{
			name: "lan_info", path: pathLanInfo,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathLanInfo, &s.LanInfo) },
			save:  func(s *Snapshot) any { return &s.LanInfo },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*LanInfo); ok {
					s.LanInfo = *p
				}
			},
		},
		{
			name: "wifi_connect_devices", path: pathWifiConnDevice,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathWifiConnDevice, &s.WifiConnDevs) },
			save:  func(s *Snapshot) any { return &s.WifiConnDevs },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*WifiConnectDevices); ok {
					s.WifiConnDevs = *p
				}
			},
		},
		{
			name: "wifi_list", path: pathWifiList,
			fetch: func(ctx context.Context, s *Snapshot) error { return c.call(ctx, pathWifiList, &s.WifiList) },
			save:  func(s *Snapshot) any { return &s.WifiList },
			restore: func(s *Snapshot, v any) {
				if p, ok := v.(*WifiList); ok {
					s.WifiList = *p
				}
			},
		},
	}
}

// FetchAll 并发采集全部端点并返回快照。
//
// 旧实现是十次串行 client.Get——一次 scrape 的延迟是十个 RTT 之和，
// 路由器忙的时候直接顶穿 Prometheus 的 scrape_timeout。这里并发发起，
// 整体耗时约等于最慢的那一个端点。
//
// 任一端点失败都不影响其它端点，也不影响本次返回：失败端点回退到上一次
// 成功的值并标记 Stale。调用方永远拿到一份非空快照。
func (c *Client) FetchAll(ctx context.Context) *Snapshot {
	begin := time.Now()

	// 定期把之前判定为「不存在」的端点放回探测名单。
	// 少了这一步，自适应降级会退化成自适应遗忘：固件升级后新长出来的接口永远采不到。
	if revived := c.profile.RecheckDue(); len(revived) > 0 {
		c.logger.Info("重新探测此前判定为不存在的端点", "endpoints", revived)
	}

	eps := c.enabledEndpoints()
	results := make([]EndpointState, len(eps))

	snap := &Snapshot{
		CollectedAt: time.Now(),
		Endpoint:    make(map[string]EndpointState, len(eps)),
	}

	var wg sync.WaitGroup
	for i, ep := range eps {
		wg.Add(1)
		// 禁裸 go func：用 async 的 GoSafe 系列，某个端点解析出意外 panic
		// 时不会带走整个 scrape 流程。
		async.GoSafeWithContextLabeled(ctx, "miwifi/"+ep.name, func(gctx context.Context) {
			defer wg.Done()

			start := time.Now()
			err := ep.fetch(gctx, snap)

			st := EndpointState{Elapsed: time.Since(start), Err: err}
			if err != nil {
				// 固件压根没有这个接口：404 一次就确定，停掉别再问。
				if stderrors.Is(err, ErrEndpointMissing) {
					c.profile.Disable(ep.name)
					c.logger.Info("该固件不提供此端点，已停止采集",
						"endpoint", ep.name, "profile", c.profile.Name())
				}

				// 超时只计数，不据此停问。
				//
				// 这里原本有个「连续 N 次超时就停问」的启发式，真机实测证明
				// 它是有害的：wifi_list 会触发 WiFi 扫描，扫描期间路由器阻塞
				// 其它所有 LuCI 请求，于是九个本来 0.2s 就返回的端点跟着一起
				// 超时，然后被这条启发式**连带误杀**。实测 8 次抓取里有 7 个
				// 端点被误关（含 status / lan_info / device_list 等确定可用的）。
				//
				// 超时无法区分「这个端点坏了」和「路由器被别的端点堵住了」，
				// 因此只作为观测信号暴露出来，停问只认 404/501 这种确定性信号。
				c.profile.RecordResult(ep.name, stderrors.Is(err, context.DeadlineExceeded))

				st.Stale = c.restoreLastGood(ep.name, snap, ep)
				// 这里刻意不打日志：十个端点各自打一行，路由器一挂就是
				// 每轮 10 行，30s 轮询下一昼夜两万行。失败信息由本函数
				// 结尾统一汇总成一行输出。
			} else {
				st.OK = true
				c.profile.RecordResult(ep.name, false)
				c.saveLastGood(ep.name, snap, ep)
			}
			results[i] = st
		})
	}
	wg.Wait()

	for i, ep := range eps {
		snap.Endpoint[ep.name] = results[i]
	}
	c.logSummary(ctx, snap, time.Since(begin))
	return snap
}

// logSummary 汇总输出本轮采集结果，把十个端点的成败压成一行。
//
// 为什么要有这一层：路由器宕机时十个端点会同时失败，逐个打 WARN 就是
// 每轮十行。在 poll 模式下 30s 一轮，一昼夜两万行——真正需要看的那条
// 「哪些端点挂了」反而被淹没了。
//
// 相同错误还会被去重合并：路由器宕机时九个端点的错误文本**完全一样**，
// 照原样拼起来单行能到 1.4KB，九遍重复同一句话。合并后形如
//
//	endpoints=[lan_info, status, …] reasons=[路由器返回非 2xx: … ×9]
//
// 去重只影响日志可读性，details 的完整逐端点信息用 DEBUG 级保留。
func (c *Client) logSummary(ctx context.Context, snap *Snapshot, elapsed time.Duration) {
	failed := snap.FailedEndpoints()
	if len(failed) == 0 {
		c.logger.CtxDebug(ctx, "采集完成", "elapsed", elapsed, "endpoints", len(snap.Endpoint))
		return
	}

	// 按错误文本归并：同一原因只说一次，并记录波及了几个端点。
	order := make([]string, 0, len(failed))
	byReason := make(map[string][]string, len(failed))
	for _, name := range failed {
		st := snap.Endpoint[name]
		reason := errText(st.Err)
		if st.Stale {
			reason += "（沿用上次成功值）"
		}
		if _, seen := byReason[reason]; !seen {
			order = append(order, reason)
		}
		byReason[reason] = append(byReason[reason], name)
	}

	reasons := make([]string, 0, len(order))
	for _, reason := range order {
		eps := byReason[reason]
		text := reason
		if len(eps) > 1 {
			text += " ×" + strconv.Itoa(len(eps))
		} else {
			text += " [" + eps[0] + "]"
		}
		reasons = append(reasons, text)
	}

	c.logger.CtxWarn(ctx, "采集部分失败",
		"elapsed", elapsed,
		"failed", len(failed),
		"total", len(snap.Endpoint),
		"endpoints", failed,
		"reasons", reasons)

	// 逐端点全量详情留给 DEBUG：排查时需要，正常运行时纯属噪声。
	for _, name := range failed {
		st := snap.Endpoint[name]
		c.logger.CtxDebug(ctx, "端点采集失败",
			"endpoint", name, "err", errText(st.Err), "using_stale_value", st.Stale)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// fetchWanInfo 处理 wan_info 的「info 字段类型不稳定」问题。
//
// info 在 WAN 未连接时是 JSON false、连接时是对象，因此先用 WanInfo 接住
// 再二次反序列化。WAN 掉线时必须显式清空，否则会把上一次的地址继续导出，
// 让人以为 WAN 还挂着。
func (c *Client) fetchWanInfo(ctx context.Context, s *Snapshot) error {
	var outer WanInfo
	if err := c.call(ctx, pathWanInfo, &outer); err != nil {
		return err
	}

	if _, isBool := outer.Info.(bool); isBool {
		s.WanInfo = Info{}
		return nil
	}
	raw, err := json.Marshal(outer.Info)
	if err != nil {
		return oerr.Wrap(err, "序列化 WAN 信息失败")
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return oerr.Wrap(err, "反序列化 WAN 信息失败")
	}
	s.WanInfo = info
	return nil
}

// saveLastGood 留档本轮成功的字段值。
func (c *Client) saveLastGood(name string, s *Snapshot, ep endpoint) {
	c.goodMu.Lock()
	defer c.goodMu.Unlock()
	c.lastGood[name] = ep.save(s)
}

// restoreLastGood 把留档写回字段，报告是否确有留档可用。
func (c *Client) restoreLastGood(name string, s *Snapshot, ep endpoint) bool {
	c.goodMu.Lock()
	defer c.goodMu.Unlock()

	v, ok := c.lastGood[name]
	if !ok {
		return false
	}
	ep.restore(s, v)
	return true
}

// ResetLastGood 清空留档，仅供测试使用。
func (c *Client) ResetLastGood() {
	c.goodMu.Lock()
	defer c.goodMu.Unlock()
	c.lastGood = make(map[string]any)
}
