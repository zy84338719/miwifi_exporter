package collector_test

import (
	"context"
	"testing"
	"time"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

// TestEndpointDisabledMetric 钉住 endpoint_disabled 指标的三种状态。
//
// 为什么需要这个指标：endpoint_supported=0 只说明「没在采」，
// 而「没在采」有两种完全不同的原因：
//
//   - profile 压根不支持（这台固件就没有，正常）
//   - 跑着跑着被停掉了（404 判定 / 连续失败 / 配置排除）
//
// 第二种是运行时状态：它会变。而且 endpoint_errors_total 对已停采的端点
// **永远是 0**（端点不在采集轮次里，失败计数根本没机会递增）——
// 于是「某个端点已经坏了」这件事在 Prometheus 里无迹可寻。
func TestEndpointDisabledMetric(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	// lan_info 模拟「该固件不提供此接口」（404 一次就判定停采）。
	fake.SetMissing("api/xqnetwork/lan_info", true)

	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, fake, "r1", 5*time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	fams := gatherSet(t, set)

	// 404 判定停采的端点：supported=0 且 disabled=1。
	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_supported",
		map[string]string{"host": "r1", "endpoint": profile.EPLanInfo}); v != 0 {
		t.Errorf("lan_info 已停采，endpoint_supported 应为 0，实际 %v", v)
	}
	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_disabled",
		map[string]string{"host": "r1", "endpoint": profile.EPLanInfo, "reason": profile.ReasonNotSupported}); v != 1 {
		t.Errorf("lan_info 已被停采，endpoint_disabled 应为 1，实际 %v", v)
	}

	// profile 不支持的端点（no-scan 跳过 wifi_list）：supported=0 但 disabled=0。
	//
	// 这一条是本指标存在的全部意义：没有它，
	// 「固件本来就没有」与「跑着跑着坏了」在指标上长得一模一样。
	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_supported",
		map[string]string{"host": "r1", "endpoint": profile.EPWifiList}); v != 0 {
		t.Errorf("wifi_list 被 profile 跳过，endpoint_supported 应为 0，实际 %v", v)
	}
	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_disabled",
		map[string]string{"host": "r1", "endpoint": profile.EPWifiList, "reason": ""}); v != 0 {
		t.Errorf("wifi_list 是 profile 跳过而非停采，endpoint_disabled 应为 0，实际 %v", v)
	}

	// 正常端点：两者都应是 1/0 的正常组合。
	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_supported",
		map[string]string{"host": "r1", "endpoint": profile.EPStatus}); v != 1 {
		t.Errorf("status 正常，endpoint_supported 应为 1，实际 %v", v)
	}
	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_disabled",
		map[string]string{"host": "r1", "endpoint": profile.EPStatus, "reason": ""}); v != 0 {
		t.Errorf("status 正常，endpoint_disabled 应为 0，实际 %v", v)
	}
}

// TestSchemaDiffClassification 端点集合差异必须按成因分类。
//
// 三类差异的处置方式完全不同：配置差异要改配置，故障差异要修机器，
// 而固件差异**什么都不用做**——那台机器本来就没有这个接口。
//
// 早先它们混在一起报 WARN 并建议「统一 profile」，而异构机型下
// 集合完全一致根本做不到，强行统一的唯一办法是丢掉真实存在的数据。
func TestSchemaDiffClassification(t *testing.T) {
	t.Run("配置差异进 Warnings", func(t *testing.T) {
		fakeA := testutil.NewFakeRouter(t)
		fakeB := testutil.NewFakeRouter(t)

		specA := newSpec(t, fakeA, "a", 5*time.Second)
		specB := newSpec(t, fakeB, "b", 5*time.Second)
		specB.ExcludeEndpoints = []string{profile.EPLanInfo}

		set, err := collector.NewSet([]collector.RouterSpec{specA, specB},
			collector.SetOptions{Namespace: "miwifi", Version: "test"})
		if err != nil {
			t.Fatalf("NewSet 失败: %v", err)
		}
		t.Cleanup(func() { _ = set.Close(context.Background()) })

		warnings := set.SchemaWarnings()
		if len(warnings) == 0 || !containsStr(warnings[0], profile.EPLanInfo) {
			t.Errorf("配置排除造成的差异应报出 lan_info，实际: %v", warnings)
		}
		if notes := set.SchemaNotes(); len(notes) != 0 {
			t.Errorf("配置差异不该出现在 Notes 里，实际: %v", notes)
		}
	})

	t.Run("固件差异进 Notes", func(t *testing.T) {
		fakeA := testutil.NewFakeRouter(t)
		fakeB := testutil.NewFakeRouter(t)
		// 这台的固件没有 lan_info（404 判定停采）。
		fakeB.SetMissing("api/xqnetwork/lan_info", true)

		specA := newSpec(t, fakeA, "a", 5*time.Second)
		specB := newSpec(t, fakeB, "b", 5*time.Second)

		set, err := collector.NewSet([]collector.RouterSpec{specA, specB},
			collector.SetOptions{Namespace: "miwifi", Version: "test"})
		if err != nil {
			t.Fatalf("NewSet 失败: %v", err)
		}
		t.Cleanup(func() { _ = set.Close(context.Background()) })

		// 先采一轮，让 404 的判定生效（停采是采集时判定的）。
		gatherSet(t, set)

		notes := set.SchemaNotes()
		if len(notes) == 0 || !containsStr(notes[0], profile.EPLanInfo) {
			t.Errorf("固件差异应出现在 Notes 里，实际: %v", notes)
		}
		if warnings := set.SchemaWarnings(); len(warnings) != 0 {
			t.Errorf("固件差异不该进 Warnings，实际: %v", warnings)
		}
	})
}

// TestSchemaAlignedIgnoresExtraEndpoints 「多出来」不算不对齐。
//
// 不同型号的路由器固件能力本就不同（实测 Home-GFW 的 R3P 不提供
// newstatus / pppoe_status，Home-RA70 的 RA70 两者都有）。
// schemaAligned 表达的是「跨台对比时这台会不会少东西」，
// 而不是「集合是否完全一样」——后者在异构场景下恒为 false，
// 于是「两台口径不同」被当成缺陷反复提示，而它其实是事实。
func TestSchemaAlignedIgnoresExtraEndpoints(t *testing.T) {
	fakeA := testutil.NewFakeRouter(t)
	fakeB := testutil.NewFakeRouter(t)

	specA := newSpec(t, fakeA, "a", 5*time.Second) // no-scan
	specB := newSpec(t, fakeB, "b", 5*time.Second) // no-scan
	specB.Profile = profile.Default                // b 多一个 wifi_list

	set, err := collector.NewSet([]collector.RouterSpec{specA, specB},
		collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	for _, r := range set.Status() {
		if !r.SchemaAligned {
			t.Errorf("%s 多采了端点，不应被判为不对齐（差异: %v）", r.Host, r.SchemaDiff)
		}
	}

	// 差异本身仍然要看得见：Status 里必须带 schemaDiff。
	var found bool
	for _, r := range set.Status() {
		for _, d := range r.SchemaDiff {
			if containsStr(d, profile.EPWifiList) {
				found = true
			}
		}
	}
	if !found {
		t.Error("schemaDiff 里应能看出 wifi_list 的差异")
	}
}
