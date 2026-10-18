package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helloworlde/miwifi-exporter/internal/collector"
	"github.com/helloworlde/miwifi-exporter/internal/config"
)

func TestLoadConfigFlagOverrides(t *testing.T) {
	const missing = "/nonexistent/config.json"

	tests := []struct {
		name      string
		env       map[string]string
		overrides config.Overrides
		want      func(*testing.T, *config.Config)
		wantErr   bool
	}{
		{
			name: "flag 覆盖环境变量",
			env: map[string]string{
				"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw",
				"METRICS_NAMESPACE": "from-env", "COLLECT_MODE": "pull",
			},
			overrides: config.Overrides{Namespace: "from-flag", CollectMode: "poll"},
			want: func(t *testing.T, c *config.Config) {
				if c.Namespace != "from-flag" {
					t.Errorf("Namespace = %q，期望 from-flag", c.Namespace)
				}
				if c.CollectMode != "poll" {
					t.Errorf("CollectMode = %q，期望 poll", c.CollectMode)
				}
			},
		},
		{
			name:      "flag 指定的指标路径会被归一化",
			env:       map[string]string{"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw"},
			overrides: config.Overrides{MetricsPath: "custom/metrics/"},
			want: func(t *testing.T, c *config.Config) {
				if c.MetricsPath != "/custom/metrics" {
					t.Errorf("MetricsPath = %q，期望 /custom/metrics", c.MetricsPath)
				}
			},
		},
		{
			name:      "flag 给出非法采集模式要报错",
			env:       map[string]string{"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw"},
			overrides: config.Overrides{CollectMode: "push"},
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := loadConfig(missing, tc.overrides)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际成功: %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig 失败: %v", err)
			}
			tc.want(t, cfg)
		})
	}
}

// TestBuildSpecsMultiRouter 覆盖规格翻译：按台覆盖优先于全局。
//
// 走 config.Load 的真实路径而不是手工拼 Config——Router 里的
// Timeout / interval 是加载时解析出来的字段，跳过加载器拼出来的
// 对象与线上行为不一致，这种测试等于在测一个不存在的世界。
func TestBuildSpecsMultiRouter(t *testing.T) {
	clearRouterEnv(t)

	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{
	  "namespace": "miwifi",
	  "routers": [
	    {"ip":"192.168.31.1","password":"a","host":"r1"},
	    {"ip":"10.10.30.248","password":"b","host":"r2",
	     "profile":"minimal","collectMode":"poll","interval":"15s","timeout":"8s"}
	  ]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	specs, err := buildSpecs(cfg)
	if err != nil {
		t.Fatalf("buildSpecs 失败: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("规格数 = %d，期望 2", len(specs))
	}

	// r1：全部沿用全局默认。
	if specs[0].Host != "r1" || specs[0].Profile != "auto" || specs[0].Mode != "pull" {
		t.Errorf("r1 未沿用全局默认: %+v", specs[0])
	}
	if specs[0].BaseURL != "http://192.168.31.1" {
		t.Errorf("r1 BaseURL = %q", specs[0].BaseURL)
	}

	// r2：按台覆盖生效。
	if specs[1].Profile != "minimal" {
		t.Errorf("r2 profile = %q，期望 minimal", specs[1].Profile)
	}
	if specs[1].Mode != collector.ModePoll {
		t.Errorf("r2 mode = %q，期望 poll", specs[1].Mode)
	}
	if specs[1].Interval != 15*time.Second {
		t.Errorf("r2 interval = %v，期望 15s", specs[1].Interval)
	}
	if specs[1].Timeout != 8*time.Second {
		t.Errorf("r2 timeout = %v，期望 8s", specs[1].Timeout)
	}
	if specs[1].Password != "b" {
		t.Error("r2 密码未正确传入规格")
	}
}

// TestRouterSpecStringHidesPassword 守住日志脱敏。
func TestRouterSpecStringHidesPassword(t *testing.T) {
	r := config.Router{IP: "192.168.31.1", Password: "topsecret", Host: "r1"}
	if got := r.String(); contains(got, "topsecret") {
		t.Errorf("Router.String() 泄漏了密码: %s", got)
	}
}

func TestBuildSpecsRejectsBadEndpointTimeouts(t *testing.T) {
	cfg := &config.Config{
		RouterProfile: "auto", RouterTimeout: time.Second,
		CollectMode: "pull", CollectInterval: time.Second, Namespace: "miwifi",
		Routers: []config.Router{{IP: "1.1.1.1", Password: "p", Host: "r1",
			EndpointTimeouts: []string{"broken"}}},
	}
	_, err := buildSpecs(cfg)
	if err == nil {
		t.Fatal("非法 endpointTimeouts 应当报错")
	}
	if !contains(err.Error(), "r1") {
		t.Errorf("错误信息应指明是哪台路由器，实际: %v", err)
	}
}

func TestLoadRoutersFromEnvJSON(t *testing.T) {
	clearRouterEnv(t)
	t.Setenv("ROUTERS", `[{"ip":"1.1.1.1","password":"a","host":"r1"},
	                       {"ip":"2.2.2.2","password":"b","host":"r2","profile":"minimal"}]`)

	cfg, err := config.Load("/nonexistent.json")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(cfg.Routers) != 2 {
		t.Fatalf("路由器数 = %d，期望 2", len(cfg.Routers))
	}
	if cfg.Routers[1].Profile != "minimal" {
		t.Errorf("r2 profile = %q", cfg.Routers[1].Profile)
	}
}

func clearRouterEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ROUTER_IP", "ROUTER_PASSWORD", "ROUTER_HOST", "ROUTERS",
		"ROUTER_TIMEOUT", "ROUTER_PORT", "METRICS_PATH", "METRICS_NAMESPACE",
		"LOG_LEVEL", "SHUTDOWN_TIMEOUT", "COLLECT_MODE", "COLLECT_INTERVAL",
		"SCRAPE_TIMEOUT", "ROUTER_PROFILE", "EXCLUDE_ENDPOINTS", "ENDPOINT_TIMEOUTS",
		"ENDPOINT_TIMEOUT_THRESHOLD",
	} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("清空 %s 失败: %v", k, err)
		}
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
