// Package testutil 提供测试用的假路由器与样本数据。
//
// 它被多个 _test.go 包引用，因此是一个普通包；但 main 不导入它，
// 不会进入发布二进制。
package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// FakeRouter 模拟小米路由器的 LuCI 接口。
type FakeRouter struct {
	Server *httptest.Server

	// ValidStok 是当前有效的 stok；设为空则所有 API 返回 403。
	ValidStok string

	mu sync.Mutex
	// payloads 存放各 API 的 JSON 响应。
	payloads map[string]string
	// failures 记录每个 API 被调用的次数。
	calls map[string]int
	// loginCalls 记录登录被触发的次数。
	loginCalls int
	// failPaths 里的 API 一律返回 500。
	failPaths map[string]bool
	// missingPaths 里的 API 模拟「该固件压根没这个接口」，返回 404。
	missingPaths map[string]bool
	// delay 是每个 API 响应前的固定延迟。
	delay time.Duration
	// pendingFail 里的 API 会失败 pendingFail[path] 次后再成功。
	pendingFail map[string]int
}

// NewFakeRouter 启动一个假路由器并注册默认样本数据。
func NewFakeRouter(t *testing.T) *FakeRouter {
	t.Helper()
	return NewFakeRouterWithDelay(t, 0)
}

// NewFakeRouterWithDelay 与 NewFakeRouter 相同，但每个 API 固定延迟一段时间才响应。
//
// 用于基准与并发断言：延迟固定后，「十端点串行」与「十端点并发」的耗时差
// 是可预测的，于是并发这件事就从「文档里说有」变成「测试里断言了」。
func NewFakeRouterWithDelay(tb testing.TB, delay time.Duration) *FakeRouter {
	tb.Helper()

	f := &FakeRouter{
		delay:        delay,
		ValidStok:    "test-stok",
		payloads:     map[string]string{},
		calls:        map[string]int{},
		failPaths:    map[string]bool{},
		missingPaths: map[string]bool{},
		pendingFail:  map[string]int{},
	}
	for path, body := range SamplePayloads {
		f.payloads[path] = body
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/luci/web", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// 必须与 router 包里的两个正则严格对应：key: 'xxx', 与 deviceId = 'xxx';
		_, _ = w.Write([]byte("<html><head><script>\n" +
			"    var encrypt = { key: 'test-key-0123456789', };\n" +
			"    var deviceId = 'test-device-id';\n" +
			"</script></head></html>"))
	})
	mux.HandleFunc("/cgi-bin/luci/api/xqsystem/init_info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hardware":"R8500","romversion":"2.28.46","id":"SN0001","routername":"miwifi","newEncryptMode":1}`))
	})
	mux.HandleFunc("/cgi-bin/luci/api/xqsystem/login", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.loginCalls++
		stok := f.ValidStok
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":  0,
			"token": stok,
			"url":   "/cgi-bin/luci/;stok=" + stok + "/home",
		})
	})
	mux.HandleFunc("/cgi-bin/luci/", f.handleAPI)

	f.Server = httptest.NewServer(mux)
	tb.Cleanup(f.Server.Close)
	return f
}

// URL 返回假路由器的根地址。
func (f *FakeRouter) URL() string { return f.Server.URL }

// handleAPI 处理带 stok 的业务 API。
func (f *FakeRouter) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	const prefix = "/cgi-bin/luci/;stok="
	if len(path) <= len(prefix) || path[:len(prefix)] != prefix {
		http.NotFound(w, r)
		return
	}
	rest := path[len(prefix):]
	sep := strings.IndexByte(rest, '/')
	if sep < 0 || sep == len(rest)-1 {
		http.NotFound(w, r)
		return
	}
	stok := rest[:sep]
	apiPath := rest[sep+1:]

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			// 客户端已放弃：不再写响应，让连接尽快释放。
			return
		}
	}

	// 一次取齐所有需要的状态，然后立刻解锁再写响应。
	//
	// 写入动作一律放到解锁之后：在持锁区里 return 会把锁带出函数，
	// 之后所有 Calls()/SetXxx() 都会永久阻塞。
	f.mu.Lock()
	f.calls[apiPath]++
	validStok := f.ValidStok
	body, hasBody := f.payloads[apiPath]
	missing := f.missingPaths[apiPath]
	forceFail := f.failPaths[apiPath]
	if n := f.pendingFail[apiPath]; n > 0 {
		f.pendingFail[apiPath] = n - 1
		forceFail = true
	}
	f.mu.Unlock()

	if missing {
		// 固件压根没有这个接口时的标准响应：404。
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":-1,"msg":"no such api"}`))
		return
	}
	if stok != validStok {
		// stok 不匹配：路由器在 stok 失效时回 403。
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":-1}`))
		return
	}
	if forceFail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
		return
	}
	if !hasBody {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":-1}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// SetPayload 覆盖某个 API 的响应体。
func (f *FakeRouter) SetPayload(apiPath, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payloads[apiPath] = body
}

// RotateStok 模拟「路由器重启」：换一个新 stok，旧 stok 立刻失效。
//
// 这比 SetStok("") 更贴近真实——路由器重启后登录会返回一个**新的** stok，
// 而不是空 stok。SetStok("") 会让登录响应里带上 ;stok=/home 这种解析不了的
// 值，测到的是「解析失败」而不是「stok 过期后自愈」。
func (f *FakeRouter) RotateStok() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	if _, err := fmt.Sscanf(f.ValidStok, "stok-%d", &n); err != nil {
		n = 0
	}
	f.ValidStok = fmt.Sprintf("stok-%d", n+1)
	return f.ValidStok
}

// SetStok 作废当前 stok（设为空表示所有 API 都将 403）。
func (f *FakeRouter) SetStok(stok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ValidStok = stok
}

// SetFail 让某个 API 持续返回 500。
func (f *FakeRouter) SetFail(apiPath string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPaths[apiPath] = fail
}

// SetMissing 模拟「该固件不提供这个接口」（返回 404）。
func (f *FakeRouter) SetMissing(apiPath string, missing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missingPaths[apiPath] = missing
}

// FailTimes 让某个 API 先失败 n 次，之后恢复正常。
func (f *FakeRouter) FailTimes(apiPath string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingFail[apiPath] = n
}

// Calls 返回某个 API 被调用的次数。
func (f *FakeRouter) Calls(apiPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[apiPath]
}

// LoginCalls 返回登录被触发的次数。
func (f *FakeRouter) LoginCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCalls
}

// SamplePayloads 是各 API 的样本响应，覆盖所有被采集的端点。
var SamplePayloads = map[string]string{
	"api/misystem/status": `{
		"code": 0,
		"mem": {"usage": 0.375, "total": "256MB", "hz": "0", "type": "DDR3"},
		"temperature": 45,
		"count": {"all": 12, "online": 9, "all_without_mash": 11, "online_without_mash": 8},
		"hardware": {"mac": "AA:BB:CC:DD:EE:FF", "platform": "R8500", "version": "2.28.46", "channel": "release", "sn": "SN0001"},
		"upTime": "86400",
		"cpu": {"core": 4, "hz": "1.2GHz", "load": 0.18},
		"wan": {"downspeed": "2048.00", "upspeed": "1024.00", "upload": "998877", "download": "887766",
			"maxdownloadspeed": "2048.00", "maxuploadspeed": "1024.00", "history": "", "devname": "WAN"},
		"dev": [
			{"mac": "11:22:33:44:55:66", "devname": "phone", "upload": 1024, "download": 2048,
				"upspeed": 10, "downspeed": 20, "online": "600", "maxdownloadspeed": "0", "maxuploadspeed": "0"},
			{"mac": "AA:BB:CC:DD:EE:FF", "devname": "laptop", "upload": "2048", "download": "4096",
				"upspeed": 0, "downspeed": 0, "online": "120", "maxdownloadspeed": "0", "maxuploadspeed": "0"}
		]
	}`,

	"api/misystem/devicelist": `{
		"code": 0, "mac": "AA:BB:CC:DD:EE:FF",
		"list": [
			{"mac": "11:22:33:44:55:66", "name": "phone", "oname": "phone", "isap": 0, "online": 1, "times": 3, "type": 0,
			 "authority": {"wan": 0, "pridisk": 0, "admin": 0, "lan": 0}, "push": 0, "parent": "AA:BB:CC:DD:EE:FF", "icon": "phone",
			 "ip": [{"ip": "192.168.31.100", "downspeed": "20", "upspeed": "10", "online": "600", "active": 1}],
			 "statistics": {"downspeed": "20", "upspeed": "10", "online": "600"}},
			{"mac": "AA:BB:CC:DD:EE:FF", "name": "laptop", "oname": "laptop", "isap": 1, "online": 1, "times": 10, "type": 0,
			 "authority": {"wan": 0, "pridisk": 0, "admin": 0, "lan": 0}, "push": 0, "parent": "", "icon": "computer",
			 "ip": [{"ip": "192.168.31.101", "downspeed": "0", "upspeed": "0", "online": "120", "active": 1}],
			 "statistics": {"downspeed": "0", "upspeed": "0", "online": "120"}}
		]
	}`,

	"api/misystem/newstatus": `{
		"bsd": "miwifi", "code": 0, "count": 12, "cap_count": 1, "re_count": 2,
		"2g": {"online_sta_count": 3}, "5g": {"online_sta_count": 5}, "5gh": {"online_sta_count": 1}
	}`,

	"api/misystem/device_list": `{
		"code": 0, "wanRX": 0,
		"devices": [
			{"mac": "11:22:33:44:55:66", "name": "phone", "oname": "phone", "isap": 0,
			 "connectionType": 3, "wifi_quality": 4, "ip": "192.168.31.100", "pcontrol": {"enabled": 1, "total": 2}},
			{"mac": "AA:BB:CC:DD:EE:FF", "name": "", "oname": "laptop", "isap": 1,
			 "connectionType": 1, "wifi_quality": 3, "ip": "192.168.31.101", "pcontrol": {"enabled": 0, "total": 0}}
		]
	}`,

	"api/xqnetwork/wan_info": `{
		"code": 0,
		"info": {
			"mac": "AA:BB:CC:DD:EE:FF", "mtu": "1500", "gateWay": "192.168.31.1",
			"dnsAddrs": "192.168.31.1", "dnsAddrs1": "223.5.5.5", "status": 1, "uptime": 3600, "ipv6_show": 1, "link": 1,
			"details": {"username": "user", "ifname": "eth2", "wanType": "pppoe", "service": "internet", "password": "x", "peerdns": "dns"},
			"ipv4": [{"ip": "192.168.31.200", "mask": "255.255.255.0"}],
			"ipv6_info": {"wanType": "dhcp", "ifname": "eth2", "dns": [], "ip6addr": ["240e:3b1:1234::1"],
				"peerdns": "dns", "lan_ip6prefix": [], "lan_ip6addr": []}
		}
	}`,

	"api/xqnetwork/wifi_detail_all": `{
		"bsd": 1, "code": 0,
		"info": [
			{"ifname": "wifi0", "ssid": "MyWiFi", "status": "1", "mode": "11ac", "encryption": "WPA2PSK", "bandwidth": "80",
			 "channelInfo": {"bandwidth": "80", "bandList": ["2.4", "5"], "channel": 149}, "signal": 50},
			{"ifname": "wifi1", "ssid": "MyWiFi_5G", "status": "0", "mode": "11ac", "encryption": "WPA2PSK", "bandwidth": "80",
			 "channelInfo": {"bandwidth": "80", "bandList": ["5"], "channel": 157}, "signal": 40}
		]
	}`,

	"api/xqnetwork/pppoe_status": `{
		"code": 0, "status": 2, "wanSpeed": "1000", "gw": "192.168.31.1", "proto": "pppoe",
		"dns": ["223.5.5.5", "119.29.29.29"], "ip": {"mask": "255.255.255.0", "address": "100.64.0.2"}
	}`,

	"api/xqnetwork/lan_info": `{
		"code": 0, "linkList": [1, 0, 1, 0],
		"info": {"status": 1, "mac": "AA:BB:CC:DD:EE:FF", "uptime": 7200, "ipv4": [{"ip": "192.168.31.1", "mask": "255.255.255.0"}]}
	}`,

	"api/xqnetwork/wifi_connect_devices": `{
		"code": 0,
		"list": [
			{"mac": "11:22:33:44:55:66", "signal": 88, "wifiIndex": 1},
			{"mac": "AA:BB:CC:DD:EE:FF", "signal": 72, "wifiIndex": 2},
			{"mac": "77:88:99:AA:BB:CC", "signal": 61, "wifiIndex": 3}
		]
	}`,

	"api/xqnetwork/wifi_list": `{
		"code": 0,
		"list": [
			{"band": "2.4G", "ssid": "Neighbor", "bssid": "DD:DD:DD:DD:DD:DD", "rssi": -55, "channel": 6, "signal": 3, "quality": 3, "encryption": "WPA2PSK"},
			{"band": "5G", "ssid": "Neighbor5G", "bssid": "EE:EE:EE:EE:EE:EE", "rssi": -70, "channel": 36, "signal": 2, "quality": 2, "encryption": "WPA2PSK"}
		]
	}`,
}
