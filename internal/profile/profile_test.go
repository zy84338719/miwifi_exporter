package profile_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
)

func TestBuiltinProfilesRegistered(t *testing.T) {
	want := []string{profile.Auto, profile.Default, profile.Minimal, profile.NoXQNetwork}
	for _, name := range want {
		if name == profile.Auto {
			continue // auto 是选择模式，不是具体 profile
		}
		p, err := profile.Resolve(name)
		if err != nil {
			t.Fatalf("内置 profile %q 未注册: %v", name, err)
		}
		if p.Description() == "" {
			t.Errorf("profile %q 缺少说明", name)
		}
	}
}

func TestEndpointSets(t *testing.T) {
	tests := []struct {
		profile     string
		wantHas     []string
		wantMissing []string
	}{
		{
			profile: profile.Default,
			wantHas: []string{
				profile.EPStatus, profile.EPDeviceList, profile.EPNewStatus,
				profile.EPDeviceListV2, profile.EPWanInfo, profile.EPWifiDetailAll,
				profile.EPPppoeStatus, profile.EPLanInfo, profile.EPWifiConnDevices,
				profile.EPWifiList,
			},
		},
		{
			profile:     profile.Minimal,
			wantHas:     []string{profile.EPStatus, profile.EPDeviceList},
			wantMissing: []string{profile.EPNewStatus, profile.EPWanInfo, profile.EPWifiList},
		},
		{
			profile:     profile.NoXQNetwork,
			wantHas:     []string{profile.EPStatus, profile.EPDeviceList, profile.EPNewStatus, profile.EPDeviceListV2},
			wantMissing: []string{profile.EPWanInfo, profile.EPLanInfo, profile.EPWifiList},
		},
	}

	for _, tc := range tests {
		t.Run(tc.profile, func(t *testing.T) {
			p, err := profile.Resolve(tc.profile)
			if err != nil {
				t.Fatalf("Resolve 失败: %v", err)
			}
			for _, ep := range tc.wantHas {
				if !p.Supports(ep) {
					t.Errorf("profile %s 应当支持端点 %s", tc.profile, ep)
				}
			}
			for _, ep := range tc.wantMissing {
				if p.Supports(ep) {
					t.Errorf("profile %s 不应支持端点 %s", tc.profile, ep)
				}
			}
		})
	}
}

func TestHasherSelection(t *testing.T) {
	p, _ := profile.Resolve(profile.Default)

	tests := []struct {
		mode    int
		wantErr bool
	}{
		{mode: 0, wantErr: false}, // SHA1
		{mode: 1, wantErr: false}, // SHA256
		{mode: 2, wantErr: true},  // 未来模式，当前实现应当明确报错而不是猜
		{mode: -1, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(strings.Repeat("mode", 1)+string(rune('0'+tc.mode)), func(t *testing.T) {
			h, err := p.Hasher(tc.mode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mode=%d 应当报错，实际返回了 hasher", tc.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("mode=%d 意外报错: %v", tc.mode, err)
			}
			if got := h("pw", "key", "nonce"); got == "" {
				t.Errorf("mode=%d 的散列结果为空", tc.mode)
			}
		})
	}

	// 散列必须是确定性的，且随输入变化——否则等于没加密。
	h0, _ := p.Hasher(0)
	h1, _ := p.Hasher(1)
	if h0("pw", "key", "n") != h0("pw", "key", "n") {
		t.Error("散列结果不确定")
	}
	if h0("pw", "key", "n") == h0("pw2", "key", "n") {
		t.Error("散列结果与密码无关")
	}
	if h0("pw", "key", "n") == h1("pw", "key", "n") {
		t.Error("SHA1 与 SHA256 结果相同，散列模式未生效")
	}
}

func TestBandFor(t *testing.T) {
	tri, _ := profile.Resolve(profile.Default)
	dual, _ := profile.Resolve(profile.Minimal)

	tests := []struct {
		name string
		p    profile.Profile
		idx  int
		want string
	}{
		{"三频段 2.4G", tri, 1, "2.4GHz"},
		{"三频段 5G", tri, 2, "5GHz"},
		{"三频段 5G-high", tri, 3, "5GHz-high"},
		{"三频段越界", tri, 9, "unknown"},
		{"双频段 2.4G", dual, 1, "2.4GHz"},
		{"双频段 5G", dual, 2, "5GHz"},
		// 关键差异：双频段机器上 wifiIndex=3 不应被标成 5GHz-high
		{"双频段无 5G-high", dual, 3, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.BandFor(tc.idx); got != tc.want {
				t.Errorf("BandFor(%d) = %q，期望 %q", tc.idx, got, tc.want)
			}
		})
	}
}

// TestLoginPageParser 覆盖标准解析器，以及「页面结构变了要报错而不是 panic」。
func TestLoginPageParser(t *testing.T) {
	tests := []struct {
		name    string
		html    string
		want    profile.LoginPageInfo
		wantErr bool
	}{
		{
			name: "标准 LuCI 页面",
			html: "<html><head><script>\n\tvar encrypt = { key: 'abc123', };\n\tvar deviceId = 'DEV001';\n</script></head></html>",
			want: profile.LoginPageInfo{Key: "abc123", DeviceID: "DEV001"},
		},
		{
			name: "Windows 换行",
			html: "<script>\r\n  var encrypt = { key: 'k9', };\r\n  var deviceId = 'D9';\r\n</script>",
			want: profile.LoginPageInfo{Key: "k9", DeviceID: "D9"},
		},
		{
			name:    "结构变化 → 报错而非 panic",
			html:    "<html><body>please enable javascript</body></html>",
			wantErr: true,
		},
		{
			name:    "只有 key 没有 deviceId",
			html:    "<script>var encrypt = { key: 'k1', };</script>",
			wantErr: true,
		},
		{
			name:    "空页面",
			html:    "",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := profile.DefaultLuCIParser().Parse(tc.html)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错，实际解析出 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("解析结果 = %+v，期望 %+v", got, tc.want)
			}
		})
	}
}

// TestCustomParserIsPlugable 验证「新固件 = 加一个 parser」这条路真的走得通。
func TestCustomParserIsPlugable(t *testing.T) {
	// 模拟一种全新的登录页结构。
	newFirmwareHTML := `<html><script>
		window.__CFG__ = { "authKey": "zzz999" };
		var devId = "NEW-FW-001";
	</script></html>`

	parser, err := profile.NewLuCIParser(
		regexp.MustCompile(`"authKey":\s*"(.*?)"`),
		regexp.MustCompile(`var devId = "(.*?)"`),
		"新固件登录页",
	)
	if err != nil {
		t.Fatalf("构造自定义 parser 失败: %v", err)
	}

	got, err := parser.Parse(newFirmwareHTML)
	if err != nil {
		t.Fatalf("自定义 parser 解析失败: %v", err)
	}
	if got.Key != "zzz999" || got.DeviceID != "NEW-FW-001" {
		t.Errorf("解析结果 = %+v", got)
	}

	// 标准 parser 应当在新页面上失败而不是给出错误结果——这正是
	// 「两者要共存」的必要性。
	if _, err := profile.DefaultLuCIParser().Parse(newFirmwareHTML); err == nil {
		t.Error("标准 parser 不应在新固件页面上解析成功")
	}
}

func TestNewLuCIParserRejectsNilRegex(t *testing.T) {
	if _, err := profile.NewLuCIParser(nil, regexp.MustCompile(`x`), ""); err == nil {
		t.Error("nil 正则应当报错")
	}
	if _, err := profile.NewLuCIParser(regexp.MustCompile(`x`), nil, ""); err == nil {
		t.Error("nil 正则应当报错")
	}
}

func TestExtractStok(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"标准形式", "/cgi-bin/luci/;stok=abc123/home", "abc123", false},
		{"带前导路径", "/foo/bar/;stok=xyz/index", "xyz", false},
		{"没有 stok", "/cgi-bin/luci/home", "", true},
		{"空串", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := profile.ExtractStok(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractStok 失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("ExtractStok(%q) = %q，期望 %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestInstanceCapabilityLifecycle 覆盖运行期的自适应降级状态机。
func TestInstanceCapabilityLifecycle(t *testing.T) {
	inst, err := profile.Select(profile.Default)
	if err != nil {
		t.Fatalf("Select 失败: %v", err)
	}

	if got := len(inst.EnabledEndpoints()); got != len(profile.AllEndpoints) {
		t.Fatalf("初始启用端点数 = %d，期望 %d", got, len(profile.AllEndpoints))
	}

	// profile 静态层面不支持的端点，永远不启用。
	if inst.Enabled(profile.EPWifiList) == false {
		// default 支持 wifi_list，下面这条用于确认 Enabled 同时受两层控制
		t.Log("default profile 未启用 wifi_list（与预期不符，继续）")
	}

	inst.Disable(profile.EPWifiList)
	if inst.Enabled(profile.EPWifiList) {
		t.Error("Disable 之后端点仍处于启用状态")
	}
	if got := inst.DisabledEndpoints(); len(got) != 1 || got[0] != profile.EPWifiList {
		t.Errorf("DisabledEndpoints = %v", got)
	}

	inst.Enable(profile.EPWifiList)
	if !inst.Enabled(profile.EPWifiList) {
		t.Error("Enable 之后端点仍处于禁用状态")
	}
}

func TestInstanceRecheck(t *testing.T) {
	inst, _ := profile.Select(profile.Default)
	inst.Disable(profile.EPLanInfo)

	// 刚禁用 → 不该立刻复探
	if revived := inst.RecheckDue(); len(revived) != 0 {
		t.Errorf("刚禁用的端点不应立即复探，实际 %v", revived)
	}
}

func TestSelectDefaults(t *testing.T) {
	for _, name := range []string{"", profile.Auto} {
		inst, err := profile.Select(name)
		if err != nil {
			t.Fatalf("Select(%q) 失败: %v", name, err)
		}
		if inst.Name() != profile.Default {
			t.Errorf("Select(%q) = %s，期望回落到 default", name, inst.Name())
		}
	}

	if _, err := profile.Select("does-not-exist"); err == nil {
		t.Error("未知 profile 应当报错")
	} else if !strings.Contains(err.Error(), "可用") {
		t.Errorf("错误信息应列出可用 profile，实际: %v", err)
	}
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	p, _ := profile.Resolve(profile.Default)
	if err := profile.Register(p); err == nil {
		t.Error("重复注册同名 profile 应当报错")
	}
}

func TestDescribeEndpoints(t *testing.T) {
	inst, _ := profile.Select(profile.Default)
	if got := inst.DescribeEndpoints(); !strings.Contains(got, "全部") {
		t.Errorf("未禁用时描述 = %q", got)
	}

	inst.Disable(profile.EPWifiList)
	got := inst.DescribeEndpoints()
	if !strings.Contains(got, profile.EPWifiList) || !strings.Contains(got, "已跳过") {
		t.Errorf("禁用后描述应点名该端点，实际 = %q", got)
	}
}

func TestRecheckIntervalIsSane(t *testing.T) {
	// 复探间隔如果配成 0 或负数，自适应就退化成「每次都试」或「永不试」。
	if profile.RecheckInterval() <= 0 {
		t.Fatalf("复探间隔必须为正，实际 %v", profile.RecheckInterval())
	}
	if profile.RecheckInterval() < time.Minute {
		t.Fatalf("复探间隔过短（%v），会退化成反复探测", profile.RecheckInterval())
	}
}

// TestRepeatedFailureDegradesEndpoint 钉住「重复失败降级」。
//
// 真实案例：R3P 上 pppoe_status 连着失败、但又不是 404，自适应探测因此
// 永远不收敛，scrape_total{result="partial"} 只增不减，一次干净的 success
// 都不会出现，而 endpoint_supported 始终说 1。
func TestRepeatedFailureDegradesEndpoint(t *testing.T) {
	inst := newTestInstance(t, profile.Default)

	const errText = "路由器返回非 2xx status=500 body=internal error"
	for i := 1; i < profile.RepeatedFailureThreshold; i++ {
		if inst.NoteRepeatedFailure(profile.EPPppoeStatus, errText) {
			t.Fatalf("第 %d 次就要求停问，阈值 %d 形同虚设",
				i, profile.RepeatedFailureThreshold)
		}
		if inst.Enabled(profile.EPPppoeStatus) != true {
			t.Fatalf("第 %d 次后端点不应被停问", i)
		}
	}

	if !inst.NoteRepeatedFailure(profile.EPPppoeStatus, errText) {
		t.Fatal("连续相同错误达到阈值后应要求停问")
	}
	inst.Disable(profile.EPPppoeStatus)
	if inst.Enabled(profile.EPPppoeStatus) {
		t.Error("停问后 Enabled 应为 false")
	}
}

// TestVaryingErrorsDoNotAccumulate 是这条降级路径最重要的安全阀。
//
// 判据是「错误文本完全相同」。如果换成「失败次数」，一次路由器重启或
// 固件升级期间的连续抖动就会把健康端点误杀——这正是超时降级翻过的车。
func TestVaryingErrorsDoNotAccumulate(t *testing.T) {
	inst := newTestInstance(t, profile.Default)

	// 错误文本每次都不同：连续 20 次也不该触发停问。
	for i := 0; i < profile.RepeatedFailureThreshold*4; i++ {
		if inst.NoteRepeatedFailure(profile.EPStatus, fmt.Sprintf("错误 %d", i)) {
			t.Fatalf("第 %d 次错误文本变化，却触发了停问", i)
		}
	}
	if !inst.Enabled(profile.EPStatus) {
		t.Error("错误文本持续变化时端点必须保持启用")
	}
}

// TestSuccessResetsRepeatedFailure 保证断续的失败不构成「压根没有」的证据。
func TestSuccessResetsRepeatedFailure(t *testing.T) {
	inst := newTestInstance(t, profile.Default)

	const errText = "boom"
	for i := 0; i < profile.RepeatedFailureThreshold-1; i++ {
		inst.NoteRepeatedFailure(profile.EPStatus, errText)
	}

	// 成功一轮：连续计数清零。
	inst.RecordSuccess(profile.EPStatus)
	if got := inst.RepeatedFailuresOf(profile.EPStatus); got != 0 {
		t.Errorf("成功后连续失败计数应清零，实际 %d", got)
	}

	// 中间成功过一次之后，同样的失败次数不再构成停问理由。
	for i := 1; i < profile.RepeatedFailureThreshold; i++ {
		if inst.NoteRepeatedFailure(profile.EPStatus, errText) {
			t.Fatalf("计数已清零，第 %d 次不该触发停问", i)
		}
	}
}

// TestRepeatedFailureKeyTruncated 守住比较键的长度上限。
//
// 某些固件会把整页 HTML 塞进错误消息。不截断的话 map 的键会变成几百 KB
// 的字符串——每台机器每个端点各一份。
func TestRepeatedFailureKeyTruncated(t *testing.T) {
	inst := newTestInstance(t, profile.Default)

	huge := strings.Repeat("A", 100000)
	for i := 1; i < profile.RepeatedFailureThreshold; i++ {
		if inst.NoteRepeatedFailure(profile.EPStatus, huge) {
			t.Fatalf("第 %d 次不该触发停问", i)
		}
	}
	// 前缀相同、后缀不同的两条超长文本必须被认作同一个错误：
	// 截断后比较键一致，连续计数继续累积，第 5 次触发。
	if !inst.NoteRepeatedFailure(profile.EPStatus, huge+"B") {
		t.Error("截断后前缀相同的两条超长错误应视为同一个，并累积到阈值")
	}
}

// TestRepeatedFailureClearedOnRecheck 保证复探时计数一并清空，
// 否则一个曾被停问的端点复探一次就会立刻再次触发停问。
func TestRepeatedFailureClearedOnRecheck(t *testing.T) {
	inst := newTestInstance(t, profile.Default)

	const errText = "boom"
	for i := 0; i < profile.RepeatedFailureThreshold-1; i++ {
		inst.NoteRepeatedFailure(profile.EPStatus, errText)
	}
	inst.Disable(profile.EPStatus)

	// RetryDisabled 以毫秒为单位比较，这里给一个足够小但非零的 ttl，
	// 再睡过 1ms 让 now - disabledAt 真的达到阈值。
	time.Sleep(2 * time.Millisecond)
	revived := inst.RetryDisabled(1)
	if len(revived) != 1 || revived[0] != profile.EPStatus {
		t.Fatalf("复探应放回 status，实际 %v", revived)
	}
	if got := inst.RepeatedFailuresOf(profile.EPStatus); got != 0 {
		t.Errorf("复探后连续失败计数应清零，实际 %d", got)
	}
}

// newTestInstance 构造一个默认 profile 实例，失败即 t.Fatal。
func newTestInstance(t *testing.T, name string) *profile.Instance {
	t.Helper()
	inst, err := profile.Select(name)
	if err != nil {
		t.Fatalf("Select(%q) 失败: %v", name, err)
	}
	return inst
}

// TestPinnedEndpointSurvivesRecheck 是本次真机事故的直接回归测试。
//
// R3P 的 pppoe_status 在 compose 里写了 excludeEndpoints，启动日志也确实
// 打了「按配置排除端点 endpoints=[pppoe_status]」，看上去完全生效。
// 结果 30 分钟后的一次复探把它放回了探测名单，从此每轮 scrape 都 partial，
// 而配置里那行字从头到尾没有任何变化——配置形同虚设且毫无提示。
func TestPinnedEndpointSurvivesRecheck(t *testing.T) {
	inst := newTestInstance(t, profile.Default)
	inst.Pin(profile.EPPppoeStatus)

	if inst.Enabled(profile.EPPppoeStatus) {
		t.Fatal("钉死后端点不应启用")
	}
	if got := inst.Pinned(); len(got) != 1 || got[0] != profile.EPPppoeStatus {
		t.Errorf("Pinned() = %v，期望 [%s]", got, profile.EPPppoeStatus)
	}

	// 时间远超复探间隔，钉死的端点仍不得被放回。
	time.Sleep(2 * time.Millisecond)
	for round := 1; round <= 5; round++ {
		revived := inst.RetryDisabled(1)
		for _, ep := range revived {
			if ep == profile.EPPppoeStatus {
				t.Fatalf("第 %d 轮复探把配置排除的端点放回了探测名单", round)
			}
		}
		if inst.Enabled(profile.EPPppoeStatus) {
			t.Fatalf("第 %d 轮复探后端点被启用了，配置排除失效", round)
		}
	}

	// Enable 能解除钉死（重启进程即走这条路）。
	inst.Enable(profile.EPPppoeStatus)
	if !inst.Enabled(profile.EPPppoeStatus) {
		t.Error("Enable 后端点应恢复启用")
	}
	if got := inst.Pinned(); len(got) != 0 {
		t.Errorf("Enable 后 Pinned() 应为空，实际 %v", got)
	}
}

// TestDisableStillRevives 确认「探测结论」与「配置排除」行为不同：
// 前者必须能被复探推翻，否则固件升级后新长出来的接口永远采不到。
func TestDisableStillRevives(t *testing.T) {
	inst := newTestInstance(t, profile.Default)
	inst.Disable(profile.EPDeviceList)

	time.Sleep(2 * time.Millisecond)
	revived := inst.RetryDisabled(1)
	if len(revived) != 1 || revived[0] != profile.EPDeviceList {
		t.Fatalf("探测禁用应可被复探恢复，实际 %v", revived)
	}
	if !inst.Enabled(profile.EPDeviceList) {
		t.Error("复探后端点应恢复启用")
	}
}
