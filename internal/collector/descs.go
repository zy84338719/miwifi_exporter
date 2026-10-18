package collector

import (
	"github.com/prometheus/client_golang/prometheus"
)

// descs 持有全部指标描述符。
//
// 指标名是对外契约，本次重构未改动任何一个。
//
// 标签集合有一处刻意的变更：**所有指标都带 host 标签**。原实现里只有一半
// 指标有 host（cpu/memory/count/uptime/wan/lan/wifi_sta/mesh），另一半
// （platform/sn/mac/ipv4/device_*/wifi_detail/wifi_device_signal/
// neighbor_wifi_*）没有。
//
// 多路由器场景下，没有 host 的那半边会与另一台路由器的同名序列**真的撞车**：
// Prometheus 直接报 `collected metric ... was collected before with the same
// name and label values`。也就是说，不补 host，多路由器根本无法实现。
//
// 补上之后绝大多数 PromQL 不受影响（不写标签的匹配器照常匹配，且跨路由器
// 聚合从此成为可能）；受影响的只有「写死完整标签集」的精确匹配式与
// recording rule。迁移说明见 README「多路由器支持」。
type descs struct {
	// —— 整机 ——
	cpucores               *prometheus.Desc
	cpumhz                 *prometheus.Desc
	cpuload                *prometheus.Desc
	memoryTotalMB          *prometheus.Desc
	memoryUsageMB          *prometheus.Desc
	memoryUsage            *prometheus.Desc
	countAll               *prometheus.Desc
	countOnline            *prometheus.Desc
	countAllWithoutMash    *prometheus.Desc
	countOnlineWithoutMash *prometheus.Desc
	uptime                 *prometheus.Desc
	platform               *prometheus.Desc
	version                *prometheus.Desc
	sn                     *prometheus.Desc
	mac                    *prometheus.Desc

	// —— WAN ——
	wanUploadSpeed   *prometheus.Desc
	wanDownloadSpeed *prometheus.Desc
	wanUploadTraffic *prometheus.Desc
	wanDownTraffic   *prometheus.Desc
	ipv4             *prometheus.Desc
	ipv4Mask         *prometheus.Desc
	ipv6             *prometheus.Desc
	wanLinkStatus    *prometheus.Desc
	wanNegSpeed      *prometheus.Desc
	wanInfo          *prometheus.Desc

	// —— 终端 ——
	deviceUploadTraffic  *prometheus.Desc
	deviceUploadSpeed    *prometheus.Desc
	deviceDownTraffic    *prometheus.Desc
	deviceDownSpeed      *prometheus.Desc
	deviceOnlineTime     *prometheus.Desc
	deviceWifiQuality    *prometheus.Desc
	deviceConnectionType *prometheus.Desc
	devicePControl       *prometheus.Desc

	// —— WiFi ——
	wifiDetail       *prometheus.Desc
	wifiStaCount     *prometheus.Desc
	meshCapCount     *prometheus.Desc
	meshReCount      *prometheus.Desc
	lanPortLink      *prometheus.Desc
	lanUptime        *prometheus.Desc
	wifiDeviceSig    *prometheus.Desc
	neighborWiFiRSSI *prometheus.Desc
	neighborWiFiCnt  *prometheus.Desc
}

// newDescs 构造全部描述符。
//
// 旧实现有近半数指标的 HELP 文案是空字符串，在 Prometheus 里表现为
// `# HELP xxx` 后面什么都不跟。这里把每条都补齐——HELP 是指标契约的一部分。
func newDescs(ns string) *descs {
	n := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(ns+"_"+name, help, labels, nil)
	}

	host := []string{"host"}
	deviceLabels := []string{"host", "ip", "mac", "device_name", "is_ap"}

	return &descs{
		cpucores:               n("cpu_cores", "路由器 CPU 核心数", host...),
		cpumhz:                 n("cpu_mhz", "路由器 CPU 主频（MHz）", host...),
		cpuload:                n("cpu_load", "路由器 CPU 负载（0-1）", host...),
		memoryTotalMB:          n("memory_total_mb", "路由器内存总量（MB）", host...),
		memoryUsageMB:          n("memory_usage_mb", "路由器已用内存（MB）", host...),
		memoryUsage:            n("memory_usage", "路由器内存使用率（0-1）", host...),
		countAll:               n("count_all", "终端总数", host...),
		countOnline:            n("count_online", "在线终端数", host...),
		countAllWithoutMash:    n("count_all_without_mash", "终端总数（不含 Mesh 节点）", host...),
		countOnlineWithoutMash: n("count_online_without_mash", "在线终端数（不含 Mesh 节点）", host...),
		uptime:                 n("uptime", "路由器运行时长（秒）", host...),

		platform: n("platform", "硬件平台标识，恒为 1", "host", "platform"),
		version:  n("version", "固件版本标识，恒为 1", "host", "version"),
		sn:       n("sn", "设备序列号标识，恒为 1", "host", "sn"),
		mac:      n("mac", "设备 MAC 标识，恒为 1", "host", "mac"),

		wanUploadSpeed:   n("wan_upload_speed", "WAN 上行速率（KB/s）", host...),
		wanDownloadSpeed: n("wan_download_speed", "WAN 下行速率（KB/s）", host...),
		wanUploadTraffic: n("wan_upload_traffic", "WAN 上行累计流量（KB）", host...),
		wanDownTraffic:   n("wan_download_traffic", "WAN 下行累计流量（KB）", host...),
		ipv4:             n("ipv4", "WAN IPv4 地址，恒为 1", "host", "ipv4"),
		ipv4Mask:         n("ipv4_mask", "WAN IPv4 子网掩码长度（CIDR 前缀位数）", "host", "ipv4"),
		ipv6:             n("ipv6", "WAN IPv6 地址，恒为 1", "host", "ipv6"),
		wanLinkStatus:    n("wan_link_status", "WAN 链路状态码（2=已连接）", host...),
		wanNegSpeed:      n("wan_negotiation_speed", "WAN 协商速率（Mbps）；0 表示路由器未上报该字段", host...),
		wanInfo:          n("wan_info", "WAN 连接信息，恒为 1", "host", "wan_ip", "gateway", "dns"),

		deviceUploadTraffic:  n("device_upload_traffic", "终端累计上行流量（KB）", deviceLabels...),
		deviceUploadSpeed:    n("device_upload_speed", "终端上行速率（KB/s）", deviceLabels...),
		deviceDownTraffic:    n("device_download_traffic", "终端累计下行流量（KB）", deviceLabels...),
		deviceDownSpeed:      n("device_download_speed", "终端下行速率（KB/s）", deviceLabels...),
		deviceOnlineTime:     n("device_online_time", "终端在线时长（秒）", deviceLabels...),
		deviceWifiQuality:    n("device_wifi_quality", "终端 WiFi 信号等级（1-4）", "host", "ip", "mac", "device_name"),
		deviceConnectionType: n("device_connection_type", "终端连接类型，恒为 1", "host", "ip", "mac", "device_name", "type"),
		devicePControl:       n("device_pcontrol", "终端是否启用家长控制（0/1）", "host", "ip", "mac", "device_name"),

		wifiDetail:       n("wifi_detail", "各射频状态（1=开启）", "host", "ssid", "status", "band_list", "channel"),
		wifiStaCount:     n("wifi_sta_count", "各频段 WiFi 在线终端数", "host", "band"),
		meshCapCount:     n("mesh_cap_count", "Mesh 主节点数", host...),
		meshReCount:      n("mesh_re_count", "Mesh 子节点数", host...),
		lanPortLink:      n("lan_port_link", "LAN 口链路状态（0=断开，1=连接）", "host", "port"),
		lanUptime:        n("lan_uptime", "LAN 运行时长（秒）", host...),
		wifiDeviceSig:    n("wifi_device_signal", "WiFi 终端信号强度（0-100）", "host", "mac", "band"),
		neighborWiFiRSSI: n("neighbor_wifi_rssi", "周围 WiFi 信号强度（dBm）", "host", "ssid", "bssid", "band", "channel"),
		neighborWiFiCnt:  n("neighbor_wifi_count", "周围 WiFi 总数", host...),
	}
}

// all 返回全部描述符，供 Describe 使用。
func (d *descs) all() []*prometheus.Desc {
	return []*prometheus.Desc{
		d.cpucores, d.cpumhz, d.cpuload,
		d.memoryTotalMB, d.memoryUsageMB, d.memoryUsage,
		d.countAll, d.countOnline, d.countAllWithoutMash, d.countOnlineWithoutMash,
		d.uptime,
		d.platform, d.version, d.sn, d.mac,
		d.wanUploadSpeed, d.wanDownloadSpeed, d.wanUploadTraffic, d.wanDownTraffic,
		d.ipv4, d.ipv4Mask, d.ipv6,
		d.wanLinkStatus, d.wanNegSpeed, d.wanInfo,
		d.deviceUploadTraffic, d.deviceUploadSpeed, d.deviceDownTraffic,
		d.deviceDownSpeed, d.deviceOnlineTime,
		d.deviceWifiQuality, d.deviceConnectionType, d.devicePControl,
		d.wifiDetail, d.wifiStaCount, d.meshCapCount, d.meshReCount,
		d.lanPortLink, d.lanUptime, d.wifiDeviceSig,
		d.neighborWiFiRSSI, d.neighborWiFiCnt,
	}
}

// selfDescs 已被 internal/collector/self.go 的原生 GaugeVec/CounterVec 取代：
// 自监控指标不需要逐值手工构造，用 Prometheus 的类型化向量更短也更不容易写错。
