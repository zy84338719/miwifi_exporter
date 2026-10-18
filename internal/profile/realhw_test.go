package profile_test

import (
	"testing"
	"time"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
)

// TestNoScanProfile 真机验证得来的约束：wifi_list 会触发 WiFi 扫描，
// 扫描期间路由器阻塞其它全部 LuCI 请求。它必须能被单独摘掉。
func TestNoScanProfile(t *testing.T) {
	p, err := profile.Resolve(profile.NoScan)
	if err != nil {
		t.Fatalf("no-scan profile 未注册: %v", err)
	}
	if p.Supports(profile.EPWifiList) {
		t.Error("no-scan 不应支持 wifi_list")
	}
	// 其余端点必须一个不少，否则这个 profile 就失去了「只去掉扫描端点」的意义。
	for _, ep := range profile.AllEndpoints {
		if ep == profile.EPWifiList {
			continue
		}
		if !p.Supports(ep) {
			t.Errorf("no-scan 应当保留端点 %s", ep)
		}
	}
	if len(p.Endpoints()) != len(profile.AllEndpoints)-1 {
		t.Errorf("no-scan 端点数 = %d，期望 %d",
			len(p.Endpoints()), len(profile.AllEndpoints)-1)
	}
}

// TestDefaultProfileKeepsWifiList 保证默认行为不偷换：老用户升级后
// neighbor_wifi_* 指标不应该凭空消失（想要更快可以显式切 no-scan）。
func TestDefaultProfileKeepsWifiList(t *testing.T) {
	p, err := profile.Resolve(profile.Default)
	if err != nil {
		t.Fatalf("default profile 未注册: %v", err)
	}
	if !p.Supports(profile.EPWifiList) {
		t.Error("default 应保留 wifi_list；跳过它必须是显式的 no-scan 选择")
	}
}

// TestSlowEndpointsIsEmptyByDefault 守住上一轮的教训。
//
// 曾把 wifi_list 按「实测 13s」配成 20s 专用超时，结果扫描期间路由器阻塞
// 其它请求，九个 0.2s 就返回的端点全被拖超时，整轮抓取从 5s 变成全面失败。
// 「实测慢就放宽超时」在这里是错的：慢的代价由别人承担。
func TestSlowEndpointsIsEmptyByDefault(t *testing.T) {
	p, _ := profile.Resolve(profile.Default)
	for _, ep := range profile.AllEndpoints {
		if _, ok := p.TimeoutFor(ep); ok {
			t.Errorf("端点 %s 有内置超时覆盖；新增前请先确认它不会阻塞其它端点", ep)
		}
	}
}

// TestRecordResultDoesNotDisable 守住「超时不停问」。
//
// 超时无法区分「这个端点坏了」与「路由器被别的端点堵住了」。
// 真机上 wifi_list 一阻塞，全场一起超时——按超时停问会把健康端点连带误杀
// （实测 8 次抓取里误关 7 个，含确定可用的 status / lan_info / device_list）。
func TestRecordResultDoesNotDisable(t *testing.T) {
	inst, err := profile.SelectWith(profile.Default, 3)
	if err != nil {
		t.Fatalf("SelectWith 失败: %v", err)
	}

	for i := 0; i < 20; i++ {
		inst.RecordResult(profile.EPWifiList, true)
	}

	if !inst.Enabled(profile.EPWifiList) {
		t.Error("连续 20 次超时后端点仍必须启用——超时不是停问依据")
	}
	if got := len(inst.DisabledEndpoints()); got != 0 {
		t.Errorf("DisabledEndpoints 应为空，实际 %v", inst.DisabledEndpoints())
	}
	if inst.TotalTimeouts() != 20 {
		t.Errorf("累计超时数 = %d，期望 20", inst.TotalTimeouts())
	}
}

// TestRecordResultResetsOnSuccess 成功应清零连续计数。
func TestRecordResultResetsOnSuccess(t *testing.T) {
	inst, _ := profile.SelectWith(profile.Default, 3)

	inst.RecordResult(profile.EPLanInfo, true)
	inst.RecordResult(profile.EPLanInfo, true)
	inst.RecordResult(profile.EPLanInfo, false) // 成功一次
	inst.RecordResult(profile.EPLanInfo, true)
	inst.RecordResult(profile.EPLanInfo, true)

	if got := inst.ConsecutiveTimeouts(profile.EPLanInfo); got != 2 {
		t.Errorf("连续超时数 = %d，期望 2（成功一次后应从头计）", got)
	}
}

// TestExplicitDisableStillWorks 确认真正确定性的信号（404 触发的 Disable）不受影响。
func TestExplicitDisableStillWorks(t *testing.T) {
	inst, _ := profile.Select(profile.NoScan)

	if inst.Enabled(profile.EPWifiList) {
		t.Error("no-scan 下 wifi_list 应因 profile 而不可用")
	}
	if !inst.Enabled(profile.EPStatus) {
		t.Error("no-scan 不应影响 status")
	}
}

func TestTimeoutThresholdNormalisation(t *testing.T) {
	for _, in := range []int{0, -1} {
		inst, err := profile.SelectWith(profile.Default, in)
		if err != nil {
			t.Fatalf("SelectWith(%d) 失败: %v", in, err)
		}
		if got := inst.TimeoutThreshold(); got != profile.DefaultTimeoutThreshold {
			t.Errorf("阈值 %d 被归一化为 %d，期望 %d", in, got, profile.DefaultTimeoutThreshold)
		}
	}
	inst, _ := profile.SelectWith(profile.Default, 7)
	if got := inst.TimeoutThreshold(); got != 7 {
		t.Errorf("自定义阈值未生效: %d", got)
	}
}

func TestConsecutiveTimeoutsOf(t *testing.T) {
	inst, _ := profile.SelectWith(profile.Default, 2)

	inst.RecordResult(profile.EPWifiList, true)
	inst.RecordResult(profile.EPWifiList, true)
	inst.RecordResult(profile.EPLanInfo, true)

	got := inst.ConsecutiveTimeoutsOf()
	if len(got) != 1 || got[0] != profile.EPWifiList {
		t.Errorf("ConsecutiveTimeoutsOf = %v，期望 [wifi_list]", got)
	}
}

// TestRecheckRestoresTimeoutCount 复探后计数应清零，给它一个重新证明自己的机会。
func TestRecheckRestoresTimeoutCount(t *testing.T) {
	inst, _ := profile.Select(profile.Default)
	inst.RecordResult(profile.EPWifiList, true)
	inst.Disable(profile.EPWifiList)

	if inst.Enabled(profile.EPWifiList) {
		t.Fatal("Disable 后不应启用")
	}
	// 时间源在包内，这里只能等一下真实时间过去；1ms 太脆，用 5ms 留足余量。
	time.Sleep(5 * time.Millisecond)
	revived := inst.RetryDisabled(1)
	if len(revived) != 1 || revived[0] != profile.EPWifiList {
		t.Fatalf("复探未恢复该端点: %v", revived)
	}
	if got := inst.ConsecutiveTimeouts(profile.EPWifiList); got != 0 {
		t.Errorf("复探后连续超时数应清零，实际 %d", got)
	}
}

func TestProfilesAreOrderedStably(t *testing.T) {
	// 端点顺序决定采集启动顺序，抖动会让日志与基准不可比。
	p, _ := profile.Resolve(profile.Default)
	first := append([]string(nil), p.Endpoints()...)
	for i := 0; i < 5; i++ {
		got := p.Endpoints()
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("端点顺序不稳定: %v vs %v", first, got)
			}
		}
	}
}

func TestTimeoutForUnknownEndpoint(t *testing.T) {
	p, _ := profile.Resolve(profile.Default)
	if _, ok := p.TimeoutFor("no-such-endpoint"); ok {
		t.Error("未知端点不应有专用超时")
	}
}
