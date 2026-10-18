// Package router 是米家路由器（MiWiFi）的 API 客户端。
//
// 职责边界：只负责「拿到数据」——登录换 stok、带鉴权发请求、重试、并发聚合。
// 指标如何表达属于 internal/collector 的事，两者通过 Snapshot 解耦。
package router

// Status 对应 /api/misystem/status，路由器整机状态。
type Status struct {
	Dev []struct {
		Mac              string      `json:"mac"`
		MaxDownloadSpeed string      `json:"maxdownloadspeed"`
		Upload           interface{} `json:"upload"`
		UpSpeed          interface{} `json:"upspeed"`
		DownSpeed        interface{} `json:"downspeed"`
		Online           string      `json:"online"`
		DevName          string      `json:"devname"`
		MaxUploadSpeed   string      `json:"maxuploadspeed"`
		Download         interface{} `json:"download"`
	} `json:"dev"`
	Code int `json:"code"`
	Mem  struct {
		Usage float64 `json:"usage"`
		Total string  `json:"total"`
		Hz    string  `json:"hz"`
		Type  string  `json:"type"`
	} `json:"mem"`
	Temperature int `json:"temperature"`
	Count       struct {
		All               int `json:"all"`
		Online            int `json:"online"`
		AllWithoutMash    int `json:"all_without_mash"`
		OnlineWithoutMash int `json:"online_without_mash"`
	} `json:"count"`
	Hardware struct {
		Mac      string `json:"mac"`
		Platform string `json:"platform"`
		Version  string `json:"version"`
		Channel  string `json:"channel"`
		Sn       string `json:"sn"`
	} `json:"hardware"`
	UpTime string `json:"upTime"`
	CPU    struct {
		Core int     `json:"core"`
		Hz   string  `json:"hz"`
		Load float64 `json:"load"`
	} `json:"cpu"`
	Wan struct {
		DownSpeed        string `json:"downspeed"`
		MaxDownloadSpeed string `json:"maxdownloadspeed"`
		History          string `json:"history"`
		DevName          string `json:"devname"`
		Upload           string `json:"upload"`
		UpSpeed          string `json:"upspeed"`
		MaxUploadSpeed   string `json:"maxuploadspeed"`
		Download         string `json:"download"`
	} `json:"wan"`
}

// DeviceList 对应 /api/misystem/devicelist，终端列表（v1）。
type DeviceList struct {
	Mac  string `json:"mac"`
	List []struct {
		Mac       string `json:"mac"`
		OnNme     string `json:"oname"`
		IsAP      int    `json:"isap"`
		Parent    string `json:"parent"`
		Authority struct {
			Wan     int `json:"wan"`
			PriDisk int `json:"pridisk"`
			Admin   int `json:"admin"`
			Lan     int `json:"lan"`
		} `json:"authority"`
		Push   int    `json:"push"`
		Online int    `json:"online"`
		Name   string `json:"name"`
		Times  int    `json:"times"`
		IP     []struct {
			DownSpeed string `json:"downspeed"`
			Online    string `json:"online"`
			Active    int    `json:"active"`
			UpSpeed   string `json:"upspeed"`
			IP        string `json:"ip"`
		} `json:"ip"`
		Statistics struct {
			DownSpeed string `json:"downspeed"`
			Online    string `json:"online"`
			UpSpeed   string `json:"upspeed"`
		} `json:"statistics"`
		Icon string `json:"icon"`
		Type int    `json:"type"`
	} `json:"list"`
	Code int `json:"code"`
}

// NewStatus 对应 /api/misystem/newstatus，提供各频段 WiFi 在线终端数与 Mesh 节点数。
//
// 注意：该接口会返回各频段密码（passwd），监控场景下刻意不采集该字段。
type NewStatus struct {
	BSD      string `json:"bsd"`
	Code     int    `json:"code"`
	Count    int    `json:"count"`
	CapCount int    `json:"cap_count"` // Mesh 主节点数
	ReCount  int    `json:"re_count"`  // Mesh 子节点数
	G2       struct {
		OnlineStaCount int `json:"online_sta_count"`
	} `json:"2g"`
	G5 struct {
		OnlineStaCount int `json:"online_sta_count"`
	} `json:"5g"`
	G5H struct {
		OnlineStaCount int `json:"online_sta_count"`
	} `json:"5gh"`
}

// DeviceListV2 对应 /api/misystem/device_list，比 devicelist 提供更丰富的设备维度：
// wifi_quality（信号等级）、connectionType（连接类型）、pcontrol（家长控制）。
//
// 注意：该接口的 ip 字段是字符串（如 "192.168.31.172"），与 devicelist 的数组形式不同。
type DeviceListV2 struct {
	WanRX   int `json:"wanRX"`
	Devices []struct {
		Mac            string `json:"mac"`
		Name           string `json:"name"`
		OName          string `json:"oname"`
		IsAP           int    `json:"isap"`
		ConnectionType int    `json:"connectionType"`
		WifiQuality    int    `json:"wifi_quality"`
		IP             string `json:"ip"`
		PControl       struct {
			Enabled int `json:"enabled"`
			Total   int `json:"total"`
		} `json:"pcontrol"`
	} `json:"devices"`
	Code int `json:"code"`
}

// WanInfo 对应 /api/xqnetwork/wan_info。
//
// info 字段在 WAN 未连接时是 bool（false）、连接时是对象，因此这里用 any 承接，
// 由 FetchWanInfo 二次反序列化到内层 Info。
type WanInfo struct {
	Info any `json:"info"`
	Code int `json:"code"`
}

// Info 是 wan_info 的内层结构。
type Info struct {
	Mac     string `json:"mac"`
	Mtu     string `json:"mtu"`
	Details struct {
		Username string `json:"username"`
		IfName   string `json:"ifname"`
		WanType  string `json:"wanType"`
		Service  string `json:"service"`
		Password string `json:"password"`
		PeerDns  string `json:"peerdns"`
	} `json:"details"`
	GateWay  string `json:"gateWay"`
	DnsAddr1 string `json:"dnsAddrs1"`
	Status   int    `json:"status"`
	Uptime   int    `json:"uptime"`
	DNSAddr  string `json:"dnsAddrs"`
	Ipv6Info struct {
		WanType      string        `json:"wanType"`
		IfName       string        `json:"ifname"`
		DNS          []interface{} `json:"dns"`
		IP6Addr      []string      `json:"ip6addr"`
		PeerDns      string        `json:"peerdns"`
		LanIP6Prefix []interface{} `json:"lan_ip6prefix"`
		LanIP6Addr   []interface{} `json:"lan_ip6addr"`
	} `json:"ipv6_info"`
	Ipv6Show int `json:"ipv6_show"`
	Link     int `json:"link"`
	Ipv4     []struct {
		Mask string `json:"mask"`
		IP   string `json:"ip"`
	} `json:"ipv4"`
}

// WifiDetailAll 对应 /api/xqnetwork/wifi_detail_all，各射频的 SSID / 信道 / 状态。
type WifiDetailAll struct {
	Bsd  int `json:"bsd"`
	Info []struct {
		IfName      string `json:"ifname"`
		ChannelInfo struct {
			Bandwidth string   `json:"bandwidth"`
			BandList  []string `json:"bandList"`
			Channel   any      `json:"channel"`
		} `json:"channelInfo"`
		Encryption    string `json:"encryption"`
		Bandwidth     string `json:"bandwidth"`
		KickThreshold string `json:"kickthreshold"`
		Status        string `json:"status"`
		Mode          string `json:"mode"`
		Bsd           string `json:"bsd"`
		Ssid          string `json:"ssid"`
		WeakThreshold string `json:"weakthreshold"`
		Device        string `json:"device"`
		Ax            string `json:"ax"`
		Hidden        any    `json:"hidden"`
		Password      string `json:"password"`
		Channel       string `json:"channel"`
		TxPWR         string `json:"txpwr"`
		WeakEnable    string `json:"weakenable"`
		TxBF          string `json:"txbf"`
		Signal        int    `json:"signal"`
	} `json:"info"`
	Code int `json:"code"`
}

// PppoeStatus 对应 /api/xqnetwork/pppoe_status，提供 WAN 链路状态、协商速率、网关与 DNS。
type PppoeStatus struct {
	Code     int      `json:"code"`
	Status   int      `json:"status"`   // 2 = 已连接
	WanSpeed string   `json:"wanSpeed"` // WAN 协商速率（Mbps）
	GW       string   `json:"gw"`       // 网关
	Proto    string   `json:"proto"`    // 接入协议：dhcp / pppoe 等
	DNS      []string `json:"dns"`
	IP       struct {
		Mask    string `json:"mask"`
		Address string `json:"address"`
	} `json:"ip"`
}

// LanInfo 对应 /api/xqnetwork/lan_info，提供各 LAN 口链路状态与 LAN uptime。
type LanInfo struct {
	Code int `json:"code"`
	Info struct {
		Status int    `json:"status"`
		Mac    string `json:"mac"`
		Uptime int    `json:"uptime"`
		Ipv4   []struct {
			Mask string `json:"mask"`
			IP   string `json:"ip"`
		} `json:"ipv4"`
	} `json:"info"`
	LinkList []int `json:"linkList"` // 每个 LAN 口的链路状态（0/1），索引即端口号
}

// WifiConnectDevices 对应 /api/xqnetwork/wifi_connect_devices，
// 提供每台 WiFi 终端的信号强度（signal，0-100）与其所在频段（wifiIndex）。
type WifiConnectDevices struct {
	Code int `json:"code"`
	List []struct {
		Mac       string `json:"mac"`
		Signal    int    `json:"signal"`
		WifiIndex int    `json:"wifiIndex"` // 1=2.4G, 2=5G, 3=5Gh
	} `json:"list"`
}

// WifiList 对应 /api/xqnetwork/wifi_list，提供周围 WiFi 的扫描结果，
// 用于信道占用与干扰分析。
type WifiList struct {
	Code int `json:"code"`
	List []struct {
		Band       string `json:"band"`
		SSID       string `json:"ssid"`
		BSSID      string `json:"bssid"`
		RSSI       int    `json:"rssi"` // 信号强度（dBm，负值，越接近 0 越强）
		Channel    int    `json:"channel"`
		Signal     int    `json:"signal"`
		Quality    int    `json:"quality"`
		Encryption string `json:"encryption"`
	} `json:"list"`
}

// InitInfo 对应 /api/xqsystem/init_info，登录流程需要其中的 newEncryptMode 与 deviceId 之外的元数据。
type InitInfo struct {
	Hardware       string `json:"hardware"`
	RomVersion     string `json:"romversion"`
	SerialNumber   string `json:"id"`
	RouterName     string `json:"routername"`
	NewEncryptMode int    `json:"newEncryptMode"`
}
