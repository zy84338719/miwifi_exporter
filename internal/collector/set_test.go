package collector_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/testutil"
)

func newSpec(t *testing.T, fake *testutil.FakeRouter, host string, timeout time.Duration) collector.RouterSpec {
	t.Helper()
	return collector.RouterSpec{
		Host:             host,
		IP:               fake.URL(),
		BaseURL:          fake.URL(),
		Password:         "pw",
		Profile:          profile.NoScan,
		Timeout:          timeout,
		Mode:             "pull",
		Interval:         50 * time.Millisecond,
		TimeoutThreshold: 3,
	}
}

func gatherSet(t *testing.T, set *collector.Set) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := set.Register(reg); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	return gather(t, reg)
}

// TestSetTwoRoutersAreIsolated 是多路由器的核心断言：
// 两台的数据必须落在各自的 host 标签下，互不串台。
func TestSetTwoRoutersAreIsolated(t *testing.T) {
	fakeA := testutil.NewFakeRouter(t)
	fakeB := testutil.NewFakeRouter(t)

	// 让两台返回可区分的 CPU 核心数，便于断言没有串台。
	fakeA.SetPayload("api/misystem/status", statusWithCores(2))
	fakeB.SetPayload("api/misystem/status", statusWithCores(8))

	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, fakeA, "hostA", 5*time.Second),
		newSpec(t, fakeB, "hostB", 5*time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	fams := gatherSet(t, set)

	if v := mustLookup(t, fams, "miwifi_cpu_cores", map[string]string{"host": "hostA"}); v != 2 {
		t.Errorf("hostA cpu_cores = %v，期望 2", v)
	}
	if v := mustLookup(t, fams, "miwifi_cpu_cores", map[string]string{"host": "hostB"}); v != 8 {
		t.Errorf("hostB cpu_cores = %v，期望 8（说明两台串台了）", v)
	}

	// 自监控指标同样必须按 host 分开。
	for _, host := range []string{"hostA", "hostB"} {
		if v := mustLookup(t, fams, "miwifi_exporter_router_up", map[string]string{"host": host}); v != 1 {
			t.Errorf("%s router_up = %v，期望 1", host, v)
		}
		if _, ok := lookup(t, fams, "miwifi_exporter_endpoint_up",
			map[string]string{"host": host, "endpoint": "status"}); !ok {
			t.Errorf("%s 缺少 endpoint_up{endpoint=status}", host)
		}
	}
}

// TestSetRejectsDuplicateHost 两台配同一个 host 会让指标撞车，必须在启动时拒掉。
func TestSetRejectsDuplicateHost(t *testing.T) {
	fake := testutil.NewFakeRouter(t)

	_, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, fake, "same", time.Second),
		newSpec(t, fake, "same", time.Second),
	}, collector.SetOptions{Namespace: "miwifi"})
	if err == nil {
		t.Fatal("host 重复应当报错")
	}
	if !containsStr(err.Error(), "same") {
		t.Errorf("错误信息应指出冲突的 host: %v", err)
	}
}

// TestSetOneSlowRouterDoesNotBlockOthers 是多机场景最核心的约束，
// 也正是 wifi_list 那个教训的延伸：**一台慢的不能拖垮另一台**。
//
// 让 A 快、B 慢（B 的每个接口都阻塞），验证 A 的指标照常产出。
func TestSetOneSlowRouterDoesNotBlockOthers(t *testing.T) {
	fast := testutil.NewFakeRouterWithDelay(t, 0)
	slow := testutil.NewFakeRouterWithDelay(t, 3*time.Second)

	// 快的台给足超时；慢的台只给 1s，让它真的失败——
	// 否则「慢但不超时」是另一回事，测不到隔离性。
	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, fast, "fast", 8*time.Second),
		newSpec(t, slow, "slow", time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	start := time.Now()
	reg := prometheus.NewRegistry()
	if err := set.Register(reg); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	elapsed := time.Since(start)

	fams := gather(t, reg)
	if v := mustLookup(t, fams, "miwifi_cpu_cores", map[string]string{"host": "fast"}); v == 0 {
		t.Error("快的这台应拿到数据（真实值非 0），实际为 0")
	}
	if v := mustLookup(t, fams, "miwifi_exporter_router_up", map[string]string{"host": "fast"}); v != 1 {
		t.Errorf("快的那台 router_up = %v，期望 1——慢的那台不该拖累它", v)
	}
	// 慢的那台数据陈旧（超时），router_up 应为 0。
	if v := mustLookup(t, fams, "miwifi_exporter_router_up", map[string]string{"host": "slow"}); v != 0 {
		t.Errorf("慢的那台 router_up = %v，期望 0", v)
	}
	// 整体耗时由最慢的那台决定，但快的台必须已经拿到值（而不是被整体放弃）。
	if elapsed > 8*time.Second {
		t.Errorf("整体耗时 %v 超出预期", elapsed)
	}
	t.Logf("快 0ms + 慢 3s 的两台，整体耗时 %v；快的台 router_up 保持 1", elapsed)
}

// TestSetPerRouterTimeoutIndependent 每台的超时互不影响。
func TestSetPerRouterTimeoutIndependent(t *testing.T) {
	slow := testutil.NewFakeRouterWithDelay(t, 4*time.Second)

	// A 给 10s 超时（能扛住），B 只给 1s（扛不住）。
	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, slow, "patient", 10*time.Second),
		newSpec(t, slow, "impatient", time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	fams := gatherSet(t, set)

	if v := mustLookup(t, fams, "miwifi_exporter_router_up", map[string]string{"host": "patient"}); v != 1 {
		t.Errorf("10s 超时的那台 router_up = %v，期望 1", v)
	}
	if v := mustLookup(t, fams, "miwifi_exporter_router_up", map[string]string{"host": "impatient"}); v != 0 {
		t.Errorf("1s 超时的那台 router_up = %v，期望 0", v)
	}
}

// TestSetPollModeBackgroundPerRouter poll 模式下每台各自攒快照。
func TestSetPollModeBackgroundPerRouter(t *testing.T) {
	fakeA := testutil.NewFakeRouter(t)
	fakeB := testutil.NewFakeRouter(t)

	specA := newSpec(t, fakeA, "pollA", 5*time.Second)
	specA.Mode = "poll"
	specA.Interval = 30 * time.Millisecond

	specB := newSpec(t, fakeB, "pollB", 5*time.Second)
	specB.Mode = "pull" // 一台 poll 一台 pull，验证模式可按台不同

	set, err := collector.NewSet([]collector.RouterSpec{specA, specB}, collector.SetOptions{
		Namespace: "miwifi", Version: "test",
	})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	set.StartBackground(&fakeApp{ctx: ctx})

	deadline := time.Now().Add(3 * time.Second)
	for {
		if set.Ready(ctx) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll 实例 3s 内未就绪")
		}
		time.Sleep(10 * time.Millisecond)
	}

	fams := gatherSet(t, set)
	if v := mustLookup(t, fams, "miwifi_cpu_cores", map[string]string{"host": "pollA"}); v == 0 {
		t.Error("poll 那台应有数据")
	}
	if v := mustLookup(t, fams, "miwifi_cpu_cores", map[string]string{"host": "pollB"}); v == 0 {
		t.Error("pull 那台应有数据")
	}
}

// TestSetStatusReportsPerRouter /routers 端点的数据来源。
func TestSetStatusReportsPerRouter(t *testing.T) {
	fake := testutil.NewFakeRouter(t)

	spec := newSpec(t, fake, "r1", time.Second)
	spec.Profile = profile.Default // 用默认 profile，才能演示「显式排除某个仍支持的端点」
	spec.ExcludeEndpoints = []string{profile.EPWifiList}

	set, err := collector.NewSet([]collector.RouterSpec{spec}, collector.SetOptions{
		Namespace: "miwifi", Version: "test",
	})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	st := set.Status()
	if len(st) != 1 {
		t.Fatalf("状态条数 = %d，期望 1", len(st))
	}
	if st[0].Host != "r1" {
		t.Errorf("Host = %q", st[0].Host)
	}
	if st[0].Profile != profile.Default {
		t.Errorf("Profile = %q，期望 %s", st[0].Profile, profile.Default)
	}
	if len(st[0].Disabled) == 0 {
		t.Error("被排除的端点应出现在 disabled 里")
	}
	if !st[0].Ready {
		t.Error("pull 模式应恒为 Ready")
	}
}

func TestSetValidation(t *testing.T) {
	if _, err := collector.NewSet(nil, collector.SetOptions{}); err == nil {
		t.Error("空列表应当报错")
	}
	fake := testutil.NewFakeRouter(t)
	spec := newSpec(t, fake, "", time.Second)
	if _, err := collector.NewSet([]collector.RouterSpec{spec}, collector.SetOptions{}); err == nil {
		t.Error("缺 host 应当报错")
	}
}

// fakeApp 收集后台任务，由测试自己用 ctx 控制生命周期。
type fakeApp struct {
	ctx   context.Context
	tasks []func(context.Context)
}

func (f *fakeApp) GoBackground(name string, fn func(context.Context)) {
	f.tasks = append(f.tasks, fn)
	go fn(f.ctx)
}

func statusWithCores(cores int) string {
	return `{"code":0,"cpu":{"core":` + strconv.Itoa(cores) + `,"hz":"1.0GHz","load":0.1},
		"mem":{"usage":0.5,"total":"128MB"},"count":{"all":1,"online":1},
		"hardware":{"platform":"test","version":"1","sn":"s","mac":"m"},"upTime":"100",
		"wan":{"downspeed":"1","upspeed":"1","upload":"1","download":"1"},"dev":[]}`
}

func mustLookup(t *testing.T, fams map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	v, ok := lookup(t, fams, name, labels)
	if !ok {
		t.Fatalf("未找到 %s%v", name, labels)
	}
	return v
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

// TestSchemaDivergenceDetection 各台 profile 不同时必须被识别出来。
//
// 真机上验证过这个差异的严重性：一台用 default（含 wifi_list）、
// 一台用 no-scan 时，前者因为 WiFi 扫描阻塞整机而九个端点全挂，
// router_up=0；后者 9/9 全通。也就是说不只是 schema 不一致，
// 还可能让其中几台彻底采不到数据。
func TestSchemaDivergenceDetection(t *testing.T) {
	fakeA := testutil.NewFakeRouter(t)
	fakeB := testutil.NewFakeRouter(t)

	specA := newSpec(t, fakeA, "a", 5*time.Second) // no-scan
	specB := newSpec(t, fakeB, "b", 5*time.Second) // no-scan
	specB.Profile = profile.Default                // ← 故意不一致

	set, err := collector.NewSet([]collector.RouterSpec{specA, specB},
		collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	warnings := set.SchemaWarnings()
	if len(warnings) != 1 {
		t.Fatalf("告警数 = %d，期望 1；实际 %v", len(warnings), warnings)
	}
	for _, want := range []string{"a", "b", "no-scan", "default", profile.EPWifiList} {
		if !containsStr(warnings[0], want) {
			t.Errorf("告警 %q 未包含 %q", warnings[0], want)
		}
	}

	// Status 里应能逐台看到是否对齐。
	st := set.Status()
	if len(st) != 2 {
		t.Fatalf("状态条数 = %d", len(st))
	}
	// 第一台是基准，恒对齐；差异由基准侧标记，这里只验证字段存在且语义合理。
	for _, r := range st {
		if r.Host == "a" && !r.SchemaAligned {
			t.Error("基准台应视为对齐")
		}
	}
}

// TestNoDivergenceWhenSameProfile 各台 profile 一致时不应误报。
func TestNoDivergenceWhenSameProfile(t *testing.T) {
	specs := make([]collector.RouterSpec, 0, 3)
	for _, host := range []string{"a", "b", "c"} {
		fake := testutil.NewFakeRouter(t)
		specs = append(specs, newSpec(t, fake, host, 5*time.Second))
	}

	set, err := collector.NewSet(specs, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	if w := set.SchemaWarnings(); len(w) != 0 {
		t.Errorf("profile 一致时不应告警，实际: %v", w)
	}
	for _, r := range set.Status() {
		if !r.SchemaAligned {
			t.Errorf("%s 被误判为 schema 不一致", r.Host)
		}
	}
}

// TestExcludeOneRouterDetected 显式 exclude 造成的差异同样要报出来。
func TestExcludeOneRouterDetected(t *testing.T) {
	fakeA := testutil.NewFakeRouter(t)
	fakeB := testutil.NewFakeRouter(t)

	specA := newSpec(t, fakeA, "a", 5*time.Second)
	specA.Profile = profile.Default

	specB := newSpec(t, fakeB, "b", 5*time.Second)
	specB.Profile = profile.Default
	specB.ExcludeEndpoints = []string{profile.EPLanInfo}

	set, err := collector.NewSet([]collector.RouterSpec{specA, specB},
		collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	w := set.SchemaWarnings()
	if len(w) != 1 || !containsStr(w[0], profile.EPLanInfo) {
		t.Errorf("应报出 lan_info 的差异，实际: %v", w)
	}
}

// aggregateMetrics 是**聚合类**指标：每台路由器都必定导出，与有没有设备、
// 有几个地址无关。它们是多台对比的基础，面板默认会直接引用。
//
// 「按实体」的指标（ipv4 / device_* / wifi_detail / lan_port_link /
// neighbor_wifi_rssi / wifi_device_signal）**不在此列**：它们是每个实体一条
// 序列，没有实体就没有序列，这是 Prometheus 的固有语义，不是 bug。
var aggregateMetrics = []string{
	"miwifi_cpu_cores", "miwifi_cpu_mhz", "miwifi_cpu_load",
	"miwifi_memory_total_mb", "miwifi_memory_usage_mb", "miwifi_memory_usage",
	"miwifi_count_all", "miwifi_count_online",
	"miwifi_count_all_without_mash", "miwifi_count_online_without_mash",
	"miwifi_uptime",
	"miwifi_platform", "miwifi_version", "miwifi_sn", "miwifi_mac",
	"miwifi_wan_upload_speed", "miwifi_wan_download_speed",
	"miwifi_wan_upload_traffic", "miwifi_wan_download_traffic",
	"miwifi_wan_link_status", "miwifi_wan_negotiation_speed", "miwifi_wan_info",
	"miwifi_lan_uptime",
	"miwifi_wifi_sta_count", "miwifi_mesh_cap_count", "miwifi_mesh_re_count",
	"miwifi_neighbor_wifi_count",
}

// TestAggregateMetricsPresentForEveryRouter 守住「不同路由器抓到的信息一样」。
//
// 这条约束的由来是真机实测：两台跑同一份配置时，聚合类指标少了
// wan_negotiation_speed（其中一台没上报该字段）。跨路由器做对比的面板会
// 看到一条有、一条没有的空洞。聚合类指标必须**恒定存在**，取不到就发 0。
func TestAggregateMetricsPresentForEveryRouter(t *testing.T) {
	// 一台给全量数据，一台给「什么都没有」的响应，模拟能力最差的那台。
	full := testutil.NewFakeRouter(t)
	empty := testutil.NewFakeRouter(t)
	for apiPath := range testutil.SamplePayloads {
		empty.SetPayload(apiPath, `{"code":0}`)
	}

	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, full, "full", 5*time.Second),
		newSpec(t, empty, "empty", 5*time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	fams := gatherSet(t, set)
	for _, host := range []string{"full", "empty"} {
		for _, name := range aggregateMetrics {
			if _, ok := lookup(t, fams, name, map[string]string{"host": host}); !ok {
				t.Errorf("%s 缺少聚合指标 %s——聚合类指标必须每台都存在（取不到就发 0）", host, name)
			}
		}
	}
}

// TestEndpointErrorCounterIncrements 直接钉住「端点失败」这条路径。
//
// 这条测试是有来历的：曾经 `endpointErrors.WithLabelValues(name)` 少传了
// host 参数，只有在**真的发生端点失败**时才会 panic。而 panic 被
// concurrency/async 恢复掉，于是那台路由器的指标被静默丢弃、
// /metrics 照样 200、所有测试照样全绿。
//
// 教训是：凡是 async.GoSafe 包着的代码，光靠「测试通过」证明不了没有 panic。
// 这条测试直接断言失败计数会增长，把那条分支钉住。
func TestEndpointErrorCounterIncrements(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	fake.SetFail("api/xqnetwork/lan_info", true) // 只让一个端点失败

	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, fake, "r1", 5*time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	fams := gatherSet(t, set)

	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_errors_total",
		map[string]string{"host": "r1", "endpoint": profile.EPLanInfo}); v < 1 {
		t.Errorf("lan_info 失败了，endpoint_errors_total 应 ≥1，实际 %v", v)
	}
	// 没失败的端点不该被计数。
	if v := mustLookup(t, fams, "miwifi_exporter_endpoint_errors_total",
		map[string]string{"host": "r1", "endpoint": profile.EPStatus}); v != 0 {
		t.Errorf("status 没失败，endpoint_errors_total 应为 0，实际 %v", v)
	}
	// router_up 表达的是「这台采到数据了吗」，不是「每个端点都成功了吗」。
	// 真机上遇到过：一台 R3P 不支持 pppoe_status，7 个端点里 1 个失败就把
	// 整台判成不可达，告警一直误报——而那台的数据其实完全正常。
	if v := mustLookup(t, fams, "miwifi_exporter_router_up", map[string]string{"host": "r1"}); v != 1 {
		t.Errorf("有 8/9 端点成功时 router_up 应为 1（可采到数据），实际 %v", v)
	}
	// 聚合指标不能因为单个端点失败而消失。
	if _, ok := lookup(t, fams, "miwifi_cpu_cores", map[string]string{"host": "r1"}); !ok {
		t.Error("单个端点失败不应导致其它指标消失")
	}
}

// TestRouterUpZeroOnlyWhenNothingCollected 守住 router_up=0 的唯一含义：
// 一个端点都没采到。
func TestRouterUpZeroOnlyWhenNothingCollected(t *testing.T) {
	// 每个端点都返回空响应（code=0 但无数据）→ 端点层面「成功」，
	// 但这里要的是「全失败」的形态：让客户端拿不到任何东西。
	dead := testutil.NewFakeRouter(t)
	for apiPath := range testutil.SamplePayloads {
		dead.SetPayload(apiPath, `not json at all`)
	}

	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, dead, "dead", 5*time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	fams := gatherSet(t, set)
	if v := mustLookup(t, fams, "miwifi_exporter_router_up", map[string]string{"host": "dead"}); v != 0 {
		t.Errorf("所有端点都失败时 router_up 应为 0，实际 %v", v)
	}
}

// TestAllSelfMetricLabelSetsConsistent 防回归：自监控指标里只要有一个 vec 的
// 标签数与实际调用处不一致，就会在真机上 panic 且被 async 静默吞掉。
//
// 这里直接比对「构造时声明的标签数」与「实际写入时传的标签值个数」。
func TestAllSelfMetricLabelSetsConsistent(t *testing.T) {
	fake := testutil.NewFakeRouter(t)
	fake.SetFail("api/misystem/status", true)
	fake.SetFail("api/xqnetwork/lan_info", true)

	set, err := collector.NewSet([]collector.RouterSpec{
		newSpec(t, fake, "r1", 3*time.Second),
		newSpec(t, fake, "r2", 3*time.Second),
	}, collector.SetOptions{Namespace: "miwifi", Version: "test"})
	if err != nil {
		t.Fatalf("NewSet 失败: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })

	reg := prometheus.NewRegistry()
	if err := set.Register(reg); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	// 多轮采集：panic 只在特定分支出现，跑几轮都触发才覆盖得住。
	for i := 0; i < 3; i++ {
		if _, err := reg.Gather(); err != nil {
			t.Fatalf("第 %d 轮 Gather 失败: %v", i+1, err)
		}
	}

	fams := gather(t, reg)
	// 每台都应产出完整的一套自监控指标，说明没有 goroutine 被 panic 打断。
	for _, host := range []string{"r1", "r2"} {
		for _, name := range []string{
			"miwifi_exporter_scrape_duration_seconds",
			"miwifi_exporter_scrape_total",
			"miwifi_exporter_endpoint_up",
			"miwifi_exporter_endpoint_stale",
			"miwifi_exporter_endpoint_errors_total",
			"miwifi_exporter_endpoint_duration_seconds",
			"miwifi_exporter_snapshot_age_seconds",
			"miwifi_exporter_router_up",
		} {
			if _, ok := lookup(t, fams, name, map[string]string{"host": host}); !ok {
				t.Errorf("%s 缺少 %s{host=%s}——很可能某个 goroutine 被 panic 打断了",
					host, name, host)
			}
		}
	}
}
