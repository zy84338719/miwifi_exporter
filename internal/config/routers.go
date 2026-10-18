package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	oerr "github.com/oekit/pkg/errors"
)

// Router 是一台被采集的路由器。
//
// 之所以把这层单独抽出来：多路由器场景下，真正会因机器而异的**全部**东西
// 都在这里——地址、密码、host 标签、固件 profile、超时、排除端点。
// 采集逻辑（并发扇出、部分失败回退、自适应降级）对每台都是同一套，
// 不应该因为「有几台」而分叉。
type Router struct {
	// IP 是路由器地址，支持 "192.168.31.1" / ":8080" / "http://..." 三种写法。
	IP string `json:"ip"`
	// Password 是后台密码。
	Password string `json:"password"`
	// Host 是 host 标签的取值。
	//
	// 它同时是**实例标识**：两台路由器若配了同一个 host，导出的指标会
	// 在标签上撞车（Prometheus 报 duplicate metrics）。Load 会直接拒绝。
	Host string `json:"host"`

	// 以下都是可选的按台覆盖；留空则回落到全局值。

	// Profile 覆盖全局 ROUTER_PROFILE。
	Profile string `json:"profile,omitempty"`
	// Timeout 覆盖全局 ROUTER_TIMEOUT。
	Timeout time.Duration `json:"-"`
	// TimeoutText 是 JSON 里写时长的形式（"5s"），解析后填进 Timeout。
	TimeoutText string `json:"timeout,omitempty"`
	// CollectMode 覆盖全局 COLLECT_MODE。
	CollectMode string `json:"collectMode,omitempty"`
	// IntervalText 是 poll 模式的采集间隔（"30s"）。
	IntervalText string `json:"interval,omitempty"`
	// ExcludeEndpoints 是该台无条件跳过的端点。
	ExcludeEndpoints []string `json:"excludeEndpoints,omitempty"`
	// EndpointTimeouts 是该台的按端点超时覆盖，形如 "wifi_list=20s"。
	EndpointTimeouts []string `json:"endpointTimeouts,omitempty"`

	// interval 是 IntervalText 解析后的值。
	interval time.Duration
}

// EffectiveTimeout 返回该台生效的请求超时。
func (r Router) EffectiveTimeout(global time.Duration) time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return global
}

// EffectiveProfile 返回该台生效的 profile 名，空表示用全局默认。
func (r Router) EffectiveProfile(global string) string {
	if p := strings.TrimSpace(r.Profile); p != "" {
		return p
	}
	return global
}

// EffectiveCollectMode 返回该台生效的采集模式。
func (r Router) EffectiveCollectMode(global string) string {
	if m := strings.TrimSpace(r.CollectMode); m != "" {
		return m
	}
	return global
}

// ParsedEndpointTimeouts 解析该台的按端点超时覆盖。
func (r Router) ParsedEndpointTimeouts() (map[string]time.Duration, error) {
	return parseEndpointTimeouts(r.EndpointTimeouts, r.Host)
}

// EffectiveInterval 返回该台生效的采集间隔（仅 poll 模式有意义）。
func (r Router) EffectiveInterval(global time.Duration) time.Duration {
	if r.interval > 0 {
		return r.interval
	}
	return global
}

// BaseURL 返回该台路由器的根地址。
func (r Router) BaseURL() string { return (&Config{RouterIP: r.IP}).RouterBaseURL() }

// String 实现 fmt.Stringer：只暴露 host 与地址，绝不带密码。
func (r Router) String() string {
	return fmt.Sprintf("%s@%s(profile=%s)", r.Host, r.IP, r.EffectiveProfile("auto"))
}

// routersFile 允许配置文件里用两种写法之一给出多台路由器。
type routersFile struct {
	Routers []Router `json:"routers"`
}

// loadRouters 解析多路由器配置。
//
// 三条来源，按优先级：
//  1. 环境变量 ROUTERS（JSON 数组）—— 容器/Helm 里最常见，不用挂配置文件
//  2. 配置文件里的 "routers" 数组
//  3. 单路由器字段（ROUTER_IP / ROUTER_PASSWORD / ROUTER_HOST）—— 旧行为
func loadRouters(cfg *Config, data []byte) error {
	// 1. env ROUTERS
	if raw := strings.TrimSpace(os.Getenv("ROUTERS")); raw != "" {
		var list []Router
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return oerr.Wrap(err, "ROUTERS 环境变量必须是路由器数组的 JSON")
		}
		if len(list) == 0 {
			return oerr.NewBadRequestError("ROUTERS 环境变量给了空数组")
		}
		return assignRouters(cfg, list)
	}

	// 2. config.json 里的 routers
	if len(data) > 0 {
		var file routersFile
		if err := json.Unmarshal(data, &file); err == nil && len(file.Routers) > 0 {
			return assignRouters(cfg, file.Routers)
		}
	}

	// 3. 单路由器（旧行为）
	if cfg.RouterIP != "" || cfg.RouterPassword != "" {
		return assignRouters(cfg, []Router{{
			IP:       cfg.RouterIP,
			Password: cfg.RouterPassword,
			Host:     cfg.RouterHost,
		}})
	}
	return oerr.NewBadRequestError(
		"未配置任何路由器。三种写法任选其一：" +
			"① 环境变量 ROUTER_IP + ROUTER_PASSWORD（单台）；" +
			"② config.json 里写 routers 数组（多台）；" +
			"③ 环境变量 ROUTERS='[{\"ip\":\"...\",\"password\":\"...\"}]'（多台，JSON）")
}

// assignRouters 归一化并校验路由器列表。
func assignRouters(cfg *Config, list []Router) error {
	seen := make(map[string]int, len(list))

	for i := range list {
		r := &list[i]
		r.IP = strings.TrimSpace(r.IP)
		r.Host = strings.TrimSpace(r.Host)

		if r.IP == "" {
			return oerr.NewBadRequestError(fmt.Sprintf(
				"第 %d 台路由器缺少地址：请设置 ROUTER_IP，或在 config.json 的 routers 数组里填 ip", i+1))
		}
		if r.Password == "" {
			return oerr.NewBadRequestError(fmt.Sprintf(
				"第 %d 台路由器（%s）缺少密码：请设置 ROUTER_PASSWORD，或在 routers 数组里填 password",
				i+1, r.IP))
		}
		// host 缺省用 IP，避免用户漏填导致两台撞成同一个空标签。
		if r.Host == "" {
			r.Host = r.IP
		}
		if prev, dup := seen[r.Host]; dup {
			return oerr.NewBadRequestError(fmt.Sprintf(
				"两台路由器的 host 标签相同（%q，见第 %d 台与第 %d 台）：导出的指标会在标签上撞车，"+
					"请给每台不同的 host", r.Host, prev+1, i+1))
		}
		seen[r.Host] = i

		if t := strings.TrimSpace(r.TimeoutText); t != "" {
			d, err := time.ParseDuration(t)
			if err != nil || d <= 0 {
				return oerr.NewBadRequestError(
					fmt.Sprintf("路由器 %s 的 timeout %q 无法解析（示例：5s）", r.Host, r.TimeoutText))
			}
			r.Timeout = d
		}
		if s := strings.TrimSpace(r.IntervalText); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil || d <= 0 {
				return oerr.NewBadRequestError(
					fmt.Sprintf("路由器 %s 的 interval %q 无法解析（示例：30s）", r.Host, r.IntervalText))
			}
			r.interval = d
		}
	}

	cfg.Routers = list
	return nil
}

// RouterStrings 返回各台路由器的 host 列表，用于日志与探针。
func (c *Config) RouterStrings() []string {
	out := make([]string, 0, len(c.Routers))
	for _, r := range c.Routers {
		out = append(out, r.Host)
	}
	return out
}
