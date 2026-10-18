package collector

import (
	"context"
	"strconv"
	"testing"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
	"github.com/helloworlde/miwifi-exporter/internal/router"
)

// stubSource 是只用于构造校验的空采集链路。
type stubSource struct{}

func (stubSource) Collect(context.Context) *router.Snapshot { return nil }
func (stubSource) Mode() string                             { return "pull" }

func TestSubnetMaskToLen(t *testing.T) {
	tests := []struct {
		name    string
		mask    string
		want    int
		wantErr bool
	}{
		{name: "24 位", mask: "255.255.255.0", want: 24},
		{name: "16 位", mask: "255.255.0.0", want: 16},
		{name: "8 位", mask: "255.0.0.0", want: 8},
		{name: "32 位", mask: "255.255.255.255", want: 32},
		{name: "非连续掩码", mask: "255.0.255.0", want: 0},
		{name: "段数不足", mask: "255.255.255", wantErr: true},
		{name: "段数过多", mask: "255.255.255.255.0", wantErr: true},
		{name: "非数字", mask: "255.255.255.a", wantErr: true},
		{name: "超过 255", mask: "255.255.255.300", wantErr: true},
		{name: "空串", mask: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := subnetMaskToLen(tc.mask)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("subnetMaskToLen(%q) 应当报错，实际得到 %d", tc.mask, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("subnetMaskToLen(%q) 意外报错: %v", tc.mask, err)
			}
			if got != tc.want {
				t.Errorf("subnetMaskToLen(%q) = %d，期望 %d", tc.mask, got, tc.want)
			}
		})
	}
}

func TestCPUMHz(t *testing.T) {
	tests := []struct {
		name string
		hz   string
		want float64
	}{
		{name: "GHz 换算", hz: "1.2GHz", want: 1200},
		{name: "GHz 取整", hz: "2GHz", want: 2000},
		{name: "MHz 原样", hz: "880MHz", want: 880},
		{name: "单位缺失", hz: "1200", want: 0},
		{name: "空串", hz: "", want: 0},
		{name: "无法解析", hz: "abcMHz", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := &router.Status{}
			st.CPU.Hz = tc.hz
			if got := cpuMHz(st); got != tc.want {
				t.Errorf("cpuMHz(%q) = %v，期望 %v", tc.hz, got, tc.want)
			}
		})
	}
}

func TestMemoryTotalMB(t *testing.T) {
	tests := []struct {
		total string
		want  float64
	}{
		{"256MB", 256},
		{"1024MB", 1024},
		{"512", 512},
		{"", 0},
		{"unknown", 0},
	}
	for _, tc := range tests {
		t.Run(tc.total, func(t *testing.T) {
			st := &router.Status{}
			st.Mem.Total = tc.total
			if got := memoryTotalMB(st); got != tc.want {
				t.Errorf("memoryTotalMB(%q) = %v，期望 %v", tc.total, got, tc.want)
			}
		})
	}
}

// TestToFloat 覆盖不同固件在 status.dev 里给出的不同数字类型。
func TestToFloat(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want float64
	}{
		{"整数", 1024, 1024},
		{"int64", int64(2048), 2048},
		{"float64", 1.5, 1.5},
		{"float32", float32(2.5), 2.5},
		{"数字字符串", "4096", 4096},
		{"带小数点字符串", "1.25", 1.25},
		{"nil", nil, 0},
		{"空串", "", 0},
		{"非数字字符串", "N/A", 0},
		{"布尔", true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toFloat(tc.in); got != tc.want {
				t.Errorf("toFloat(%v) = %v，期望 %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestJoinBandList 锁住 band_list 标签的既有拼接格式。
//
// 末段补 "MHz" 是旧版行为，看着别扭，但属于既有标签取值：
// 改了会让历史面板里按 band_list 做的过滤对不上。
func TestJoinBandList(t *testing.T) {
	tests := []struct {
		name  string
		bands []string
		want  string
	}{
		{"双频段", []string{"2.4", "5"}, "2.4/5MHz"},
		{"单频段", []string{"5"}, "5MHz"},
		{"三频段", []string{"2.4", "5", "5gh"}, "2.4/5/5ghMHz"},
		{"空列表", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinBandList(tc.bands); got != tc.want {
				t.Errorf("joinBandList(%v) = %q，期望 %q", tc.bands, got, tc.want)
			}
		})
	}
}

// TestBandForDelegatesToProfile 验证频段映射确实交给了 profile，
// 而不是继续在采集层硬编码（硬编码就是版本差异漏出去的地方）。
func TestBandForDelegatesToProfile(t *testing.T) {
	tests := []struct {
		profileName string
		idx         int
		want        string
	}{
		{profile.Default, 1, "2.4GHz"},
		{profile.Default, 2, "5GHz"},
		{profile.Default, 3, "5GHz-high"},
		{profile.Default, 99, "unknown"},

		// 双频段 profile 下 wifiIndex=3 不该被映射成 5GHz-high
		{profile.Minimal, 1, "2.4GHz"},
		{profile.Minimal, 2, "5GHz"},
		{profile.Minimal, 3, "unknown"},

		{profile.Default, 0, "unknown"},
		{profile.Default, -1, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.profileName+"/"+strconv.Itoa(tc.idx), func(t *testing.T) {
			prof, err := profile.Select(tc.profileName)
			if err != nil {
				t.Fatalf("选择 profile 失败: %v", err)
			}
			c, err := New(CollectorOptions{Namespace: "miwifi", Source: stubSource{}})
			if err != nil {
				t.Fatalf("构造 Collector 失败: %v", err)
			}
			c.prof = prof
			if got := c.bandFor(tc.idx); got != tc.want {
				t.Errorf("bandFor(%d) = %q，期望 %q", tc.idx, got, tc.want)
			}
		})
	}
}

// TestParseFloat 确认解析失败静默返回 0 而不刷日志。
func TestParseFloat(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"1.5", 1.5},
		{"0", 0},
		{"-3.25", -3.25},
		{"1e3", 1000},
		{"", 0},
		{"-", 0},
		{"12,5", 0},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseFloat(tc.in); got != tc.want {
				t.Errorf("parseFloat(%q) = %v，期望 %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDescsCoverAllMetrics(t *testing.T) {
	d := newDescs("miwifi")
	all := d.all()

	seen := map[string]bool{}
	for _, desc := range all {
		if desc == nil {
			t.Fatal("descs.all() 里存在 nil")
		}
		if seen[desc.String()] {
			t.Errorf("descs.all() 里有重复描述符: %s", desc)
		}
		seen[desc.String()] = true
	}
	// legacyMetricCount = compat_test.go 里 legacyContract 的条数，
	// 即重构前 collector/collector.go 暴露的指标总数。两处必须一致，
	// 任何一侧漏登记都会在下面这句断言上暴露出来。
	const legacyMetricCount = 42
	if len(all) != legacyMetricCount {
		t.Errorf("描述符数量 = %d，与重构前的 %d 条指标契约不一致", len(all), legacyMetricCount)
	}
}
