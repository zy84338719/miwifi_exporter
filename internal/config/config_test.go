package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helloworlde/miwifi-exporter/internal/config"
)

// writeConfig 写一个临时配置文件。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}
	return path
}

// clearEnv 清掉所有会影响配置的环境变量，避免开发机上的残留值污染测试。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ROUTER_IP", "ROUTER_PASSWORD", "ROUTER_HOST", "ROUTER_TIMEOUT",
		"ROUTER_PORT", "METRICS_PATH", "METRICS_NAMESPACE", "LOG_LEVEL",
		"SHUTDOWN_TIMEOUT", "COLLECT_MODE", "COLLECT_INTERVAL", "SCRAPE_TIMEOUT",
	} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("清空环境变量 %s 失败: %v", k, err)
		}
	}
}

func TestLoadPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		env    map[string]string
		want   func(*testing.T, *config.Config)
		noFile bool
	}{
		{
			name: "配置文件提供全部必填项",
			json: `{"ip":"192.168.31.1","password":"pw","port":9100,"host":"r1"}`,
			want: func(t *testing.T, c *config.Config) {
				if c.RouterIP != "192.168.31.1" || c.RouterPassword != "pw" {
					t.Errorf("路由器配置读取错误: %+v", c)
				}
				if c.Port != 9100 {
					t.Errorf("Port = %d，期望 9100", c.Port)
				}
				if c.RouterHost != "r1" {
					t.Errorf("RouterHost = %q，期望 r1", c.RouterHost)
				}
			},
		},
		{
			name: "环境变量覆盖配置文件",
			json: `{"ip":"192.168.31.1","password":"pw","port":9100,"host":"r1"}`,
			env: map[string]string{
				"ROUTER_IP":   "10.0.0.1",
				"ROUTER_HOST": "from-env",
			},
			want: func(t *testing.T, c *config.Config) {
				if c.RouterIP != "10.0.0.1" {
					t.Errorf("ROUTER_IP 未覆盖配置文件: %q", c.RouterIP)
				}
				if c.RouterHost != "from-env" {
					t.Errorf("ROUTER_HOST 未覆盖配置文件: %q", c.RouterHost)
				}
				// 未被 env 覆盖的字段仍保留配置文件的值
				if c.Port != 9100 {
					t.Errorf("Port 应保留配置文件的 9100，实际 %d", c.Port)
				}
			},
		},
		{
			name:   "完全没有配置文件时用环境变量",
			noFile: true,
			env:    map[string]string{"ROUTER_IP": "172.16.0.1", "ROUTER_PASSWORD": "envpw"},
			want: func(t *testing.T, c *config.Config) {
				if c.RouterIP != "172.16.0.1" || c.RouterPassword != "envpw" {
					t.Errorf("环境变量未生效: %+v", c)
				}
			},
		},
		{
			name:   "默认值兜底",
			noFile: true,
			env:    map[string]string{"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw"},
			want: func(t *testing.T, c *config.Config) {
				if c.Port != 9001 {
					t.Errorf("默认端口 = %d，期望 9001", c.Port)
				}
				if c.RouterHost != "miwifi" {
					t.Errorf("默认 host = %q，期望 miwifi", c.RouterHost)
				}
				if c.MetricsPath != "/metrics" {
					t.Errorf("默认指标路径 = %q", c.MetricsPath)
				}
				if c.CollectMode != config.ModePull {
					t.Errorf("默认采集模式 = %q，期望 pull", c.CollectMode)
				}
				if c.RouterTimeout != 5*time.Second {
					t.Errorf("默认路由器超时 = %v", c.RouterTimeout)
				}
			},
		},
		{
			name:   "时长与枚举按 tag 解析",
			noFile: true,
			env: map[string]string{
				"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw",
				"COLLECT_MODE": "poll", "COLLECT_INTERVAL": "45s",
				"ROUTER_TIMEOUT": "2500ms", "LOG_LEVEL": "debug",
			},
			want: func(t *testing.T, c *config.Config) {
				if c.CollectMode != config.ModePoll {
					t.Errorf("CollectMode = %q", c.CollectMode)
				}
				if c.CollectInterval != 45*time.Second {
					t.Errorf("CollectInterval = %v，期望 45s", c.CollectInterval)
				}
				if c.RouterTimeout != 2500*time.Millisecond {
					t.Errorf("RouterTimeout = %v，期望 2.5s", c.RouterTimeout)
				}
				if c.LogLevel != "debug" {
					t.Errorf("LogLevel = %q", c.LogLevel)
				}
			},
		},
		{
			name: "指标路径归一化",
			json: `{"ip":"1.1.1.1","password":"pw"}`,
			env:  map[string]string{"METRICS_PATH": "custom/metrics/"},
			want: func(t *testing.T, c *config.Config) {
				if c.MetricsPath != "/custom/metrics" {
					t.Errorf("MetricsPath = %q，期望 /custom/metrics", c.MetricsPath)
				}
			},
		},
		{
			name: "纯斜杠的指标路径回落到默认值",
			json: `{"ip":"1.1.1.1","password":"pw"}`,
			env:  map[string]string{"METRICS_PATH": "/"},
			want: func(t *testing.T, c *config.Config) {
				if c.MetricsPath != "/metrics" {
					t.Errorf("MetricsPath = %q，期望回落为 /metrics", c.MetricsPath)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			path := filepath.Join(t.TempDir(), "missing.json")
			if !tc.noFile {
				path = writeConfig(t, tc.json)
			}

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			tc.want(t, cfg)
		})
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		noFile  bool
		env     map[string]string
		wantErr string
	}{
		{
			name:    "缺少 IP",
			json:    `{"password":"pw"}`,
			wantErr: "缺少地址",
		},
		{
			name:    "缺少密码",
			json:    `{"ip":"192.168.31.1"}`,
			wantErr: "缺少密码",
		},
		{
			name:    "端口越界",
			json:    `{"ip":"1.1.1.1","password":"pw","port":70000}`,
			wantErr: "",
		},
		{
			name:    "采集模式非法",
			noFile:  true,
			env:     map[string]string{"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw", "COLLECT_MODE": "push"},
			wantErr: "",
		},
		{
			name:    "采集间隔越界",
			noFile:  true,
			env:     map[string]string{"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw", "COLLECT_INTERVAL": "100ms"},
			wantErr: "",
		},
		{
			name:    "日志级别非法",
			noFile:  true,
			env:     map[string]string{"ROUTER_IP": "1.1.1.1", "ROUTER_PASSWORD": "pw", "LOG_LEVEL": "verbose"},
			wantErr: "",
		},
		{
			name:    "配置文件语法错误",
			json:    `{ this is not json`,
			wantErr: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			path := filepath.Join(t.TempDir(), "missing.json")
			if tc.json != "" {
				path = writeConfig(t, tc.json)
			}

			cfg, err := config.Load(path)
			if err == nil {
				t.Fatalf("期望报错，实际成功: %+v", cfg)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestPasswordNeverLeaks 是本次重构的安全底线。
//
// 旧版启动日志直接打印明文密码：
//
//	log.Println("获取到的地址: ", Configs.IP, "密码: ", Configs.Password, ...)
//
// 只要日志被采集、被转发或被截图，路由器后台权限就等于公开了。
func TestPasswordNeverLeaks(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{"ip":"1.1.1.1","password":"super-secret-password"}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	const secret = "super-secret-password"
	if got := cfg.String(); strings.Contains(got, secret) {
		t.Errorf("Config.String() 泄漏了密码: %s", got)
	}
	if got := strings.ToLower(cfg.String()); !strings.Contains(got, "***") {
		t.Errorf("Config.String() 未做脱敏: %s", got)
	}
}

func TestRouterBaseURL(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want string
	}{
		{"纯 IP", "192.168.31.1", "http://192.168.31.1"},
		{"带端口", "192.168.31.1:8080", "http://192.168.31.1:8080"},
		{"已带协议", "http://192.168.31.1", "http://192.168.31.1"},
		{"带协议与尾部斜杠", "https://router.lan/", "https://router.lan"},
		{"主机名", "router.lan", "http://router.lan"},
		{"IPv6", "fd00::1", "http://[fd00::1]:80"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{RouterIP: tc.ip}
			if got := c.RouterBaseURL(); got != tc.want {
				t.Errorf("RouterBaseURL() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestValidateRouterHost(t *testing.T) {
	tests := []struct {
		host    string
		wantErr bool
	}{
		{"miwifi", false},
		{"router-1", false},
		{"", true},
		{"has space", true},
		{`has"quote`, true},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			err := config.ValidateRouterHost(tc.host)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateRouterHost(%q) error = %v，wantErr = %v", tc.host, err, tc.wantErr)
			}
		})
	}
}

// TestParsedEndpointTimeouts 表驱动覆盖 "端点名=时长" 的解析。
func TestParsedEndpointTimeouts(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    map[string]time.Duration
		wantErr bool
	}{
		{name: "空配置", in: nil, want: nil},
		{name: "单条", in: []string{"wifi_list=20s"}, want: map[string]time.Duration{"wifi_list": 20 * time.Second}},
		{name: "多条", in: []string{"wifi_list=20s", "lan_info=8s"},
			want: map[string]time.Duration{"wifi_list": 20 * time.Second, "lan_info": 8 * time.Second}},
		{name: "带空格", in: []string{" wifi_list = 1m30s "},
			want: map[string]time.Duration{"wifi_list": 90 * time.Second}},
		{name: "跳过空项", in: []string{"", "wifi_list=5s", "  "},
			want: map[string]time.Duration{"wifi_list": 5 * time.Second}},
		{name: "缺少等号", in: []string{"wifi_list"}, wantErr: true},
		{name: "时长非法", in: []string{"wifi_list=abc"}, wantErr: true},
		{name: "时长为零", in: []string{"wifi_list=0s"}, wantErr: true},
		{name: "时长为负", in: []string{"wifi_list=-5s"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{EndpointTimeouts: tc.in}
			got, err := c.ParsedEndpointTimeouts()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("解析结果 = %v，期望 %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("[%s] = %v，期望 %v", k, got[k], v)
				}
			}
		})
	}
}

// TestParseEndpointTimeoutsErrorNamesField 错误信息要指明是哪一项写错了。
func TestParseEndpointTimeoutsErrorNamesField(t *testing.T) {
	c := &config.Config{EndpointTimeouts: []string{"lan_info=8s", "bad-entry"}}
	_, err := c.ParsedEndpointTimeouts()
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "bad-entry") {
		t.Errorf("错误信息应指出具体项，实际: %v", err)
	}
}
