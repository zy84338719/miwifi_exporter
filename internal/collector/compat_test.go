package collector_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oekit/concurrency/retry"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/router"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// metricContract 是 exporter 暴露的完整指标契约。
//
// ## 与重构前的差异（只有一处，且是必须的）
//
// **所有指标都带 host 标签**。重构前只有一半指标有 host：
//
//	有 host：cpu_* / memory_* / count_* / uptime / wan_* / lan_* / wifi_sta_count / mesh_*
//	无 host：platform / version / sn / mac / ipv4* / ipv6 / device_* / wifi_detail
//	         / wifi_device_signal / neighbor_wifi_*
//
// 为什么必须改：多路由器场景下，没有 host 的那半边会与另一台路由器的
// 同名序列**真的撞车**——Prometheus 在 Gather 阶段直接报
//
//	collected metric "miwifi_platform" {…} was collected before with
//	the same name and label values
//
// 也就是说，不补 host，多路由器功能根本无法实现。这不是取舍，是前提。
//
// ## 对现有 PromQL 的实际影响
//
//	不受影响：miwifi_cpu_cores、sum(miwifi_device_upload_traffic)、
//	          miwifi_platform{platform="RB03"} 等**不写 host** 的匹配器
//	受影响：  写死完整标签集的精确匹配，例如
//	          miwifi_device_upload_traffic{ip="x",mac="y",device_name="z",is_ap="0"}
//	新增能力：sum by (host) (...) 跨路由器聚合从此可用
var legacyContract = []struct {
	name   string
	labels []string
}{
	{"miwifi_cpu_cores", []string{"host"}},
	{"miwifi_cpu_mhz", []string{"host"}},
	{"miwifi_cpu_load", []string{"host"}},
	{"miwifi_memory_total_mb", []string{"host"}},
	{"miwifi_memory_usage_mb", []string{"host"}},
	{"miwifi_memory_usage", []string{"host"}},
	{"miwifi_count_all", []string{"host"}},
	{"miwifi_count_online", []string{"host"}},
	{"miwifi_count_all_without_mash", []string{"host"}},
	{"miwifi_count_online_without_mash", []string{"host"}},
	{"miwifi_uptime", []string{"host"}},

	// ↓ 以下五个重构前没有 host，为多路由器支持而补上
	{"miwifi_platform", []string{"host", "platform"}},
	{"miwifi_version", []string{"host", "version"}},
	{"miwifi_sn", []string{"host", "sn"}},
	{"miwifi_mac", []string{"host", "mac"}},

	{"miwifi_wan_upload_speed", []string{"host"}},
	{"miwifi_wan_download_speed", []string{"host"}},
	{"miwifi_wan_upload_traffic", []string{"host"}},
	{"miwifi_wan_download_traffic", []string{"host"}},

	// ↓ 这三个重构前没有 host
	{"miwifi_ipv4", []string{"host", "ipv4"}},
	{"miwifi_ipv4_mask", []string{"host", "ipv4"}},
	{"miwifi_ipv6", []string{"host", "ipv6"}},

	{"miwifi_device_upload_traffic", []string{"host", "ip", "mac", "device_name", "is_ap"}},
	{"miwifi_device_upload_speed", []string{"host", "ip", "mac", "device_name", "is_ap"}},
	{"miwifi_device_download_traffic", []string{"host", "ip", "mac", "device_name", "is_ap"}},
	{"miwifi_device_download_speed", []string{"host", "ip", "mac", "device_name", "is_ap"}},
	{"miwifi_device_online_time", []string{"host", "ip", "mac", "device_name", "is_ap"}},

	// ↓ 重构前没有 host
	{"miwifi_wifi_detail", []string{"host", "ssid", "status", "band_list", "channel"}},

	{"miwifi_wifi_sta_count", []string{"host", "band"}},
	{"miwifi_mesh_cap_count", []string{"host"}},
	{"miwifi_mesh_re_count", []string{"host"}},

	{"miwifi_wan_link_status", []string{"host"}},
	{"miwifi_wan_negotiation_speed", []string{"host"}},
	{"miwifi_wan_info", []string{"host", "wan_ip", "gateway", "dns"}},

	{"miwifi_lan_port_link", []string{"host", "port"}},
	{"miwifi_lan_uptime", []string{"host"}},

	// ↓ 重构前没有 host
	{"miwifi_wifi_device_signal", []string{"host", "mac", "band"}},

	// ↓ 重构前没有 host
	{"miwifi_neighbor_wifi_rssi", []string{"host", "ssid", "bssid", "band", "channel"}},
	{"miwifi_neighbor_wifi_count", []string{"host"}},

	// ↓ 这三个重构前没有 host
	{"miwifi_device_wifi_quality", []string{"host", "ip", "mac", "device_name"}},
	{"miwifi_device_connection_type", []string{"host", "ip", "mac", "device_name", "type"}},
	{"miwifi_device_pcontrol", []string{"host", "ip", "mac", "device_name"}},
}

// TestLegacyMetricContract 是本次重构的核心回归测试：
// 重构后暴露的路由器指标必须与重构前**逐条一致**。
func TestLegacyMetricContract(t *testing.T) {
	reg := newTestRegistry(t)

	got := gather(t, reg)

	for _, want := range legacyContract {
		t.Run(want.name, func(t *testing.T) {
			fam, ok := got[want.name]
			if !ok {
				t.Fatalf("指标 %s 丢失：重构后不再暴露", want.name)
			}
			if gotLabels := labelNames(fam); !equalStringSets(gotLabels, want.labels) {
				t.Fatalf("指标 %s 的标签不匹配\n 期望: %v\n 实际: %v", want.name, want.labels, gotLabels)
			}
			if len(fam.Metric) == 0 {
				t.Fatalf("指标 %s 没有任何样本", want.name)
			}
		})
	}

	// 反向检查：不得凭空多出路由器指标（自监控指标以 _exporter_ 开头，单独放行）。
	t.Run("没有多余的路由器指标", func(t *testing.T) {
		allowed := map[string]bool{}
		for _, w := range legacyContract {
			allowed[w.name] = true
		}
		for name := range got {
			if strings.Contains(name, "_exporter_") {
				continue
			}
			if !allowed[name] {
				t.Errorf("出现了旧版没有的指标 %s；新增指标请显式登记到 legacyContract", name)
			}
		}
	})
}

// TestMetricHelpNotEmpty 守住「每条指标都要有 HELP」这条新规矩。
//
// 旧实现有近半数指标的 docString 是空串，在 Prometheus 里表现为
// `# HELP xxx` 后面空白，查询时没有任何可读描述。
func TestMetricHelpNotEmpty(t *testing.T) {
	reg := newTestRegistry(t)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	for _, fam := range families {
		t.Run(fam.GetName(), func(t *testing.T) {
			if fam.GetHelp() == "" {
				t.Errorf("指标 %s 的 HELP 为空", fam.GetName())
			}
		})
	}
}

// TestMetricValues 校验一批关键指标的实际取值，确保重构没有算错。
func TestMetricValues(t *testing.T) {
	reg := newTestRegistry(t)
	got := gather(t, reg)

	tests := []struct {
		metric string
		labels map[string]string
		want   float64
	}{
		{"miwifi_cpu_cores", map[string]string{"host": "testhost"}, 4},
		{"miwifi_cpu_mhz", map[string]string{"host": "testhost"}, 1200}, // "1.2GHz" → 1200MHz
		{"miwifi_cpu_load", map[string]string{"host": "testhost"}, 0.18},
		{"miwifi_memory_total_mb", map[string]string{"host": "testhost"}, 256},
		{"miwifi_memory_usage_mb", map[string]string{"host": "testhost"}, 96}, // 0.375 * 256
		{"miwifi_memory_usage", map[string]string{"host": "testhost"}, 0.375},
		{"miwifi_count_all", map[string]string{"host": "testhost"}, 12},
		{"miwifi_count_online", map[string]string{"host": "testhost"}, 9},
		{"miwifi_count_all_without_mash", map[string]string{"host": "testhost"}, 11},
		{"miwifi_count_online_without_mash", map[string]string{"host": "testhost"}, 8},
		{"miwifi_uptime", map[string]string{"host": "testhost"}, 86400},

		{"miwifi_wan_upload_speed", map[string]string{"host": "testhost"}, 1024},
		{"miwifi_wan_download_speed", map[string]string{"host": "testhost"}, 2048},
		{"miwifi_wan_upload_traffic", map[string]string{"host": "testhost"}, 998877},
		{"miwifi_wan_download_traffic", map[string]string{"host": "testhost"}, 887766},

		{"miwifi_ipv4", map[string]string{"ipv4": "192.168.31.200"}, 1},
		{"miwifi_ipv4_mask", map[string]string{"ipv4": "192.168.31.200"}, 24},
		{"miwifi_ipv6", map[string]string{"ipv6": "240e:3b1:1234::1"}, 1},

		{"miwifi_wifi_sta_count", map[string]string{"host": "testhost", "band": "2.4GHz"}, 3},
		{"miwifi_wifi_sta_count", map[string]string{"host": "testhost", "band": "5GHz"}, 5},
		{"miwifi_wifi_sta_count", map[string]string{"host": "testhost", "band": "5GHz-high"}, 1},
		{"miwifi_mesh_cap_count", map[string]string{"host": "testhost"}, 1},
		{"miwifi_mesh_re_count", map[string]string{"host": "testhost"}, 2},

		{"miwifi_wan_link_status", map[string]string{"host": "testhost"}, 2},
		{"miwifi_wan_negotiation_speed", map[string]string{"host": "testhost"}, 1000},
		{"miwifi_lan_uptime", map[string]string{"host": "testhost"}, 7200},
		{"miwifi_neighbor_wifi_count", map[string]string{"host": "testhost"}, 2},

		// device_list 的 name 为空时回落到 oname
		{"miwifi_device_wifi_quality", map[string]string{
			"ip": "192.168.31.100", "mac": "11:22:33:44:55:66", "device_name": "phone"}, 4},
		{"miwifi_device_wifi_quality", map[string]string{
			"ip": "192.168.31.101", "mac": "AA:BB:CC:DD:EE:FF", "device_name": "laptop"}, 3},
		{"miwifi_device_pcontrol", map[string]string{
			"ip": "192.168.31.100", "mac": "11:22:33:44:55:66", "device_name": "phone"}, 1},
		{"miwifi_device_connection_type", map[string]string{
			"ip": "192.168.31.100", "mac": "11:22:33:44:55:66", "device_name": "phone", "type": "3"}, 1},

		{"miwifi_wifi_device_signal", map[string]string{"mac": "11:22:33:44:55:66", "band": "2.4GHz"}, 88},
		{"miwifi_wifi_device_signal", map[string]string{"mac": "77:88:99:AA:BB:CC", "band": "5GHz-high"}, 61},

		{"miwifi_lan_port_link", map[string]string{"host": "testhost", "port": "0"}, 1},
		{"miwifi_lan_port_link", map[string]string{"host": "testhost", "port": "1"}, 0},

		// devicelist 用 mac 关联 status.dev 补出 ip / is_ap
		{"miwifi_device_upload_traffic", map[string]string{
			"ip": "192.168.31.100", "mac": "11:22:33:44:55:66", "device_name": "phone", "is_ap": "0"}, 1024},
		{"miwifi_device_download_traffic", map[string]string{
			"ip": "192.168.31.100", "mac": "11:22:33:44:55:66", "device_name": "phone", "is_ap": "0"}, 2048},
		{"miwifi_device_online_time", map[string]string{
			"ip": "192.168.31.101", "mac": "AA:BB:CC:DD:EE:FF", "device_name": "laptop", "is_ap": "1"}, 120},

		{"miwifi_wifi_detail", map[string]string{
			"ssid": "MyWiFi", "status": "1", "band_list": "2.4/5MHz", "channel": "149"}, 1},

		{"miwifi_neighbor_wifi_rssi", map[string]string{
			"ssid": "Neighbor", "bssid": "DD:DD:DD:DD:DD:DD", "band": "2.4G", "channel": "6"}, -55},

		{"miwifi_wan_info", map[string]string{
			"host": "testhost", "wan_ip": "100.64.0.2", "gateway": "192.168.31.1",
			"dns": "223.5.5.5,119.29.29.29"}, 1},
	}

	for _, tc := range tests {
		t.Run(tc.metric+"/"+tc.labels["host"], func(t *testing.T) {
			gotVal, ok := lookup(t, got, tc.metric, tc.labels)
			if !ok {
				t.Fatalf("未找到指标 %s，标签 %v", tc.metric, tc.labels)
			}
			if gotVal != tc.want {
				t.Errorf("%s%v = %v，期望 %v", tc.metric, tc.labels, gotVal, tc.want)
			}
		})
	}
}

// TestSelfMetricsPresence 确认自监控指标族已就位。
func TestSelfMetricsPresence(t *testing.T) {
	reg := newTestRegistry(t)
	got := gather(t, reg)

	want := []string{
		"miwifi_exporter_build_info",
		"miwifi_exporter_scrape_duration_seconds",
		"miwifi_exporter_scrape_total",
		"miwifi_exporter_endpoint_up",
		"miwifi_exporter_endpoint_stale",
		"miwifi_exporter_endpoint_errors_total",
		"miwifi_exporter_endpoint_duration_seconds",
		"miwifi_exporter_snapshot_age_seconds",
		"miwifi_exporter_router_up",
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("缺少自监控指标 %s", name)
		}
	}

	if v, ok := lookup(t, got, "miwifi_exporter_router_up", nil); !ok || v != 1 {
		t.Errorf("假路由器可达时 router_up 应为 1，实际存在=%v 值=%v", ok, v)
	}
	for _, ep := range []string{"status", "wan_info", "lan_info", "wifi_list"} {
		if v, ok := lookup(t, got, "miwifi_exporter_endpoint_up", map[string]string{"endpoint": ep}); !ok || v != 1 {
			t.Errorf("端点 %s 采集成功，endpoint_up 应为 1，实际存在=%v 值=%v", ep, ok, v)
		}
	}
}

// --- helpers ---

// newTestRegistry 装配一套完整的采集链路并返回已注册的 registry。
func newTestRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()

	fake := testutil.NewFakeRouter(t)
	client, err := router.New(router.Options{
		BaseURL:  fake.URL(),
		Password: "test-password",
		Timeout:  3 * time.Second,
		Retry:    retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 router.Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	c := newCollector(t, client, "miwifi", "testhost")

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("注册 Collector 失败: %v", err)
	}
	if err := reg.Register(c.Self()); err != nil {
		t.Fatalf("注册自监控指标失败: %v", err)
	}

	// 触发一次采集，让 pull 模式真的去打过假路由器。
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	return reg
}

// gatherRegistry 把 Collector 与其自监控指标注册进一个独立 registry。
func gatherRegistry(t *testing.T, c *collector.Collector) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("注册 Collector 失败: %v", err)
	}
	if err := reg.Register(c.Self()); err != nil {
		t.Fatalf("注册自监控指标失败: %v", err)
	}
	return reg
}

func newCollector(t *testing.T, client *router.Client, ns, host string) *collector.Collector {
	c, err := collector.New(collector.CollectorOptions{
		Namespace: ns,
		Host:      host,
		Version:   "test",
		Source: collector.NewPullSource(collector.PullSourceOptions{
			Client:  client,
			Timeout: 5 * time.Second,
		}),
	})
	if err != nil {
		t.Fatalf("构造 Collector 失败: %v", err)
	}
	return c
}

func gather(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

// labelNames 返回该指标族的标签名（按声明顺序，非字典序）。
func labelNames(fam *dto.MetricFamily) []string {
	if fam.GetMetric() == nil {
		return nil
	}
	return labelNamesOf(fam.Metric[0])
}

func labelNamesOf(m *dto.Metric) []string {
	pairs := m.GetLabel()
	out := make([]string, 0, len(pairs))
	for _, lp := range pairs {
		out = append(out, lp.GetName())
	}
	return out
}

// lookup 按标签子集查找单个样本值。
func lookup(t *testing.T, families map[string]*dto.MetricFamily, name string, want map[string]string) (float64, bool) {
	t.Helper()

	fam, ok := families[name]
	if !ok {
		return 0, false
	}
	for _, m := range fam.GetMetric() {
		if matchLabels(m, want) {
			switch {
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

func matchLabels(m *dto.Metric, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalStringSets 按集合比较标签名。
//
// 不比较顺序：prometheus 的 Gather 出口端点返回的 LabelPair 已按名字排序，
// 声明顺序并不会出现在 /metrics 的文本格式里，因此顺序不是对外契约的一部分。
// 真正必须守死的是标签**名字集合**——多一个少一个都会让 PromQL 直接报错。
func equalStringSets(a, b []string) bool {
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	return equalStrings(sa, sb)
}

// TestEndpointSupportedIsCorrectOnFirstGather 守住「endpoint_supported 不能滞后一拍」。
//
// 该指标过去是在 Collector.Collect 里顺带更新的，而注册表不保证两个
// collector 的收集顺序，于是第一次抓取拿到的是预置的初始值（全 1），
// 第二次才正确。现场表现就是「刚重启时所有端点都显示 supported」，
// 很容易被误读成 profile 没生效。
func TestEndpointSupportedIsCorrectOnFirstGather(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	client, err := router.New(router.Options{
		BaseURL:     fake.URL(),
		Password:    "pw",
		Timeout:     3 * time.Second,
		ProfileName: profile.Minimal, // 只有 status + devicelist
		Retry:       retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	c, err := collector.New(collector.CollectorOptions{
		Namespace: "miwifi",
		Host:      "testhost",
		Version:   "test",
		Profile:   client.Profile(),
		Source: collector.NewPullSource(collector.PullSourceOptions{
			Client: client, Timeout: 5 * time.Second,
		}),
	})
	if err != nil {
		t.Fatalf("构造 Collector 失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := reg.Register(c.Self()); err != nil {
		t.Fatalf("注册自监控指标失败: %v", err)
	}

	// 注意：这里只 Gather 一次就直接断言，不做任何预热。
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	var supported *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "miwifi_exporter_endpoint_supported" {
			supported = f
		}
	}
	if supported == nil {
		t.Fatal("缺少 miwifi_exporter_endpoint_supported")
	}

	want := map[string]float64{
		profile.EPStatus:     1,
		profile.EPDeviceList: 1,
		profile.EPWanInfo:    0,
		profile.EPLanInfo:    0,
		profile.EPWifiList:   0,
	}
	for endpoint, wantVal := range want {
		var got float64 = -1
		for _, m := range supported.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "endpoint" && lp.GetValue() == endpoint {
					got = m.GetGauge().GetValue()
				}
			}
		}
		if got != wantVal {
			t.Errorf("第一次 Gather 时 endpoint_supported{endpoint=%q} = %v，期望 %v（说明该指标滞后了一拍）",
				endpoint, got, wantVal)
		}
	}
}

// TestEmptyOrBrokenWANAddressIsNotExported 守住「没有实体就没有序列」这条规则。
//
// 部分固件在 WAN 未连接（或用了别的接入方式）时，wan_info 的 ipv4 数组里
// 会带一条 ip/mask 全为空的占位记录；也有固件给了 IP 却不给 mask。
// 旧实现照单全收，导出的是：
//
//	miwifi_ipv4{host="h",ipv4=""} 1              ← 空标签，无意义的序列
//	miwifi_ipv4_mask{host="h",ipv4=""} 0          ← 0 会被读成 /0 网段
//
// 真机上这会让每轮采集都刷一条 WARN，且 Prometheus 里长期挂着
// 一条地址为空的「WAN」，比缺一条序列更难排查。
func TestEmptyOrBrokenWANAddressIsNotExported(t *testing.T) {
	tests := []struct {
		name string
		// ipv4 是注入到 wan_info 的 ipv4 数组（原始 JSON）。
		ipv4     string
		wantIPv4 string // 期望存在的 ipv4 标签值；空表示不该有任何 ipv4 序列
		wantMask string // 期望存在的 ipv4_mask 标签值；空表示不该有任何 mask 序列
	}{
		{
			name:     "正常地址",
			ipv4:     `[{"ip":"10.10.30.249","mask":"255.255.255.0"}]`,
			wantIPv4: "10.10.30.249",
			wantMask: "10.10.30.249",
		},
		{
			name: "IP 与 mask 全空（WAN 未连接的占位记录）",
			ipv4: `[{"ip":"","mask":""}]`,
			// 整条实体跳过：既没有 ipv4，也没有 ipv4_mask
		},
		{
			name: "只有空白字符",
			ipv4: `[{"ip":"   ","mask":"  "}]`,
		},
		{
			name: "有 IP 但没给 mask",
			ipv4: `[{"ip":"10.10.30.249","mask":""}]`,
			// IP 是有效实体的，照发；掩码无从得知，不能谎报成 0
			wantIPv4: "10.10.30.249",
		},
		{
			name:     "掩码不可解析",
			ipv4:     `[{"ip":"10.10.30.249","mask":"255.255.255"}]`,
			wantIPv4: "10.10.30.249",
		},
		{
			name:     "掩码非连续",
			ipv4:     `[{"ip":"10.10.30.249","mask":"255.0.255.0"}]`,
			wantIPv4: "10.10.30.249",
		},
		{
			name: "空地址与正常地址混在一起",
			ipv4: `[{"ip":"","mask":""},{"ip":"10.10.30.249","mask":"255.255.255.0"}]`,
			// 跳过空的那条，保留正常的那条
			wantIPv4: "10.10.30.249",
			wantMask: "10.10.30.249",
		},
		{
			name:     "ipv4 数组为空",
			ipv4:     `[]`,
			wantIPv4: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := testutil.NewFakeRouter(t)
			fake.SetPayload("api/xqnetwork/wan_info",
				`{"code":0,"info":{"ipv4":`+tc.ipv4+`,"ipv6_info":{"ip6addr":["240e::1"]},"dnsAddrs":"1.1.1.1"}}`)

			client, err := router.New(router.Options{
				BaseURL:  fake.URL(),
				Password: "test-password",
				Timeout:  3 * time.Second,
				Retry:    retry.Config{Attempts: 1},
			})
			if err != nil {
				t.Fatalf("构造 router.Client 失败: %v", err)
			}
			t.Cleanup(func() { _ = client.Close(context.Background()) })

			got := gather(t, gatherRegistry(t, newCollector(t, client, "miwifi", "testhost")))

			assertLabel := func(metric string, label string, want string) {
				t.Helper()
				series := got[metric]
				if series == nil {
					if want != "" {
						t.Fatalf("%s 完全没导出，期望至少有一条 %s=%q", metric, label, want)
					}
					return
				}
				var values []string
				for _, m := range series.GetMetric() {
					for _, lp := range m.GetLabel() {
						if lp.GetName() == label {
							values = append(values, lp.GetValue())
						}
					}
				}
				for _, v := range values {
					if v == "" {
						t.Errorf("%s 导出了空标签序列 %s=\"\"，应当整条跳过", metric, label)
					}
				}
				if want == "" {
					if len(values) != 0 {
						t.Errorf("%s 导出了 %v，期望一条都不导出", metric, values)
					}
					return
				}
				found := false
				for _, v := range values {
					if v == want {
						found = true
					}
				}
				if !found {
					t.Errorf("%s 的 %s 标签 = %v，期望包含 %q", metric, label, values, want)
				}
			}

			assertLabel("miwifi_ipv4", "ipv4", tc.wantIPv4)
			assertLabel("miwifi_ipv4_mask", "ipv4", tc.wantMask)

			// ipv6 同理：空串不是合法地址
			assertLabel("miwifi_ipv6", "ipv6", "240e::1")
		})
	}
}

// TestIPv6EmptyAddressIsSkipped 单独钉住 ipv6 的空值防护。
func TestIPv6EmptyAddressIsSkipped(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	fake.SetPayload("api/xqnetwork/wan_info",
		`{"code":0,"info":{"ipv4":[{"ip":"10.10.30.249","mask":"255.255.255.0"}],`+
			`"ipv6_info":{"ip6addr":["","240e::1","  "]}}}`)

	client, err := router.New(router.Options{
		BaseURL:  fake.URL(),
		Password: "test-password",
		Timeout:  3 * time.Second,
		Retry:    retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 router.Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	got := gather(t, gatherRegistry(t, newCollector(t, client, "miwifi", "testhost")))

	series := got["miwifi_ipv6"]
	if series == nil {
		t.Fatal("miwifi_ipv6 完全没导出")
	}
	var values []string
	for _, m := range series.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "ipv6" {
				values = append(values, lp.GetValue())
			}
		}
	}
	if len(values) != 1 || values[0] != "240e::1" {
		t.Errorf("miwifi_ipv6 导出 = %v，期望恰好一条 240e::1（空串应被跳过）", values)
	}
}

// TestIdentitylessEntityIsNotExported 守住「没有实体就没有序列」在设备侧的落实。
//
// 真机（10.10.30.71）上 10.10.30.248 会把一批设备汇总成一条
// mac="" device_name="Others" 的记录，旧实现照单全收，导出成
//
//	miwifi_device_upload_traffic{mac="",device_name="Others",ip="",is_ap=""} 2.04e+09
//
// 这条序列没有 MAC 就无法定位，还会在 sum by (mac) 时混进一个空桶。
// 真机上这两类各有 2~3 条，长期挂在 Prometheus 里。
//
// 另一半是**不该动**的：MAC 有效但查不到 IP 的终端仍要导出，
// ip="" 是在诚实地说「IP 未知」，不是「这台机器不存在」。
func TestIdentitylessEntityIsNotExported(t *testing.T) {
	fake := testutil.NewFakeRouter(t)

	// status.dev：混入一条 mac 为空的「Others」汇总记录
	fake.SetPayload("api/misystem/status", `{
		"code": 0,
		"mem": {"usage": 0.375, "total": "256MB", "hz": "1.2GHz"},
		"count": {"all": 3, "online": 3, "all_without_mash": 3, "online_without_mash": 3},
		"hardware": {"mac": "AA:BB:CC:DD:EE:FF", "platform": "R8500", "version": "2.28.46", "sn": "SN0001"},
		"upTime": "86400", "cpu": {"core": 4, "hz": "1.2GHz", "load": 0.18},
		"wan": {"downspeed": "1", "upspeed": "1", "upload": "1", "download": "1"},
		"dev": [
			{"mac": "11:22:33:44:55:66", "devname": "phone", "upload": 1024, "download": 2048},
			{"mac": "AA:BB:CC:DD:EE:FF", "devname": "laptop", "upload": 2048, "download": 4096},
			{"mac": "", "devname": "Others", "upload": 999, "download": 888},
			{"mac": "   ", "devname": "Others", "upload": 777, "download": 666}
		]
	}`)

	// device_list：混入一条 mac 为空的记录
	fake.SetPayload("api/misystem/device_list", `{
		"code": 0, "devices": [
			{"mac": "11:22:33:44:55:66", "name": "phone", "oname": "phone", "isap": 0,
			 "connectionType": 3, "wifi_quality": 4, "ip": "192.168.31.100", "pcontrol": {"enabled": 1}},
			{"mac": "", "name": "ghost", "oname": "ghost", "isap": 0,
			 "connectionType": 1, "wifi_quality": 1, "ip": "192.168.31.199", "pcontrol": {"enabled": 0}}
		]
	}`)

	// wifi_connect_devices：混入空 mac
	fake.SetPayload("api/xqnetwork/wifi_connect_devices", `{
		"code": 0, "list": [
			{"mac": "11:22:33:44:55:66", "signal": 88, "wifiIndex": 1},
			{"mac": "", "signal": 50, "wifiIndex": 2}
		]
	}`)

	// wifi_list：混入空 bssid（邻居 AP 没有身份就没法定位）
	fake.SetPayload("api/xqnetwork/wifi_list", `{
		"code": 0, "list": [
			{"band": "2.4G", "ssid": "Neighbor", "bssid": "DD:DD:DD:DD:DD:DD", "rssi": -55, "channel": 6},
			{"band": "5G", "ssid": "", "bssid": "", "rssi": -70, "channel": 36}
		]
	}`)

	client, err := router.New(router.Options{
		BaseURL:  fake.URL(),
		Password: "test-password",
		Timeout:  3 * time.Second,
		Retry:    retry.Config{Attempts: 1},
	})
	if err != nil {
		t.Fatalf("构造 router.Client 失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	got := gather(t, gatherRegistry(t, newCollector(t, client, "miwifi", "testhost")))

	assertNoEmptyIdentity := func(metric, identity string) {
		t.Helper()
		series := got[metric]
		if series == nil {
			return
		}
		for _, m := range series.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == identity && strings.TrimSpace(lp.GetValue()) == "" {
					t.Errorf("%s 导出了 %s=\"\" 的无身份序列，其余标签: %v",
						metric, identity, m.GetLabel())
				}
			}
		}
	}

	for _, metric := range []string{
		"miwifi_device_upload_traffic",
		"miwifi_device_download_traffic",
		"miwifi_device_wifi_quality",
		"miwifi_device_connection_type",
		"miwifi_device_pcontrol",
		"miwifi_wifi_device_signal",
	} {
		assertNoEmptyIdentity(metric, "mac")
	}
	assertNoEmptyIdentity("miwifi_neighbor_wifi_rssi", "bssid")

	// 「Others」那条不能混进来，但两台真机必须还在
	assertHas := func(metric string, want map[string]string) {
		t.Helper()
		series := got[metric]
		if series == nil {
			t.Fatalf("%s 完全没导出", metric)
		}
		for _, m := range series.GetMetric() {
			match := true
			for k, v := range want {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				return
			}
		}
		t.Errorf("%s 缺少实体 %v", metric, want)
	}
	assertHas("miwifi_device_upload_traffic", map[string]string{"mac": "11:22:33:44:55:66", "ip": "192.168.31.100"})
	assertHas("miwifi_device_upload_traffic", map[string]string{"mac": "AA:BB:CC:DD:EE:FF", "ip": "192.168.31.101"})
	assertHas("miwifi_device_wifi_quality", map[string]string{"mac": "11:22:33:44:55:66"})
	assertHas("miwifi_wifi_device_signal", map[string]string{"mac": "11:22:33:44:55:66"})
	assertHas("miwifi_neighbor_wifi_rssi", map[string]string{"bssid": "DD:DD:DD:DD:DD:DD"})

	// 邻居总数仍然按路由器上报的条数算，与是否导出序列无关
	if fam := got["miwifi_neighbor_wifi_count"]; fam == nil {
		t.Error("miwifi_neighbor_wifi_count 完全没导出")
	} else if v := fam.GetMetric()[0].GetGauge().GetValue(); v != 2 {
		t.Errorf("miwifi_neighbor_wifi_count = %v，期望 2（跳过无身份序列不应改变计数）", v)
	}
}
