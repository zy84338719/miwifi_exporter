// Package config 负责 miwifi-exporter 的配置加载、优先级合并与校验。
//
// 优先级（高 → 低）：命令行 flag > 环境变量 > config.json > default tag。
// flag 在 Load 之后由 main 覆盖，其余三级由本包按 env → default 的顺序填充。
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/core/config"
	oerr "github.com/oekit/pkg/errors"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
)

// 采集模式取值。
const (
	// ModePull 在每次 Prometheus scrape 时同步向路由器取值（默认，保持旧版语义）。
	ModePull = "pull"
	// ModePoll 由后台 goroutine 周期采集，scrape 只读内存快照。
	ModePoll = "poll"
)

// Config 是全部运行期配置。json tag 兼容旧版 config.json 的字段名，
// env / default / range / options tag 由 github.com/oekit/core/config 消费。
type Config struct {
	// ---- 路由器连接 ----
	RouterIP       string        `json:"ip"       env:"ROUTER_IP"`
	RouterPassword string        `json:"password" env:"ROUTER_PASSWORD"`
	RouterHost     string        `json:"host"     env:"ROUTER_HOST"   default:"miwifi"`
	RouterTimeout  time.Duration `env:"ROUTER_TIMEOUT" default:"5s" range:"(0s,60s]"`

	// ---- HTTP 服务 ----
	Port            int           `json:"port" env:"ROUTER_PORT" default:"9001" range:"[1,65535]"`
	MetricsPath     string        `env:"METRICS_PATH"     default:"/metrics"`
	Namespace       string        `env:"METRICS_NAMESPACE" default:"miwifi"`
	LogLevel        string        `env:"LOG_LEVEL"        default:"info" options:"debug,info,warn,error"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" default:"10s" range:"[0s,5m]"`

	// ---- 固件 profile ----
	// RouterProfile 指定固件差异适配方案；auto 表示交给自适应探测。
	RouterProfile string `env:"ROUTER_PROFILE" default:"auto"`
	// EndpointTimeoutThreshold 是「连续超时多少次后停问该端点」。
	EndpointTimeoutThreshold int `env:"ENDPOINT_TIMEOUT_THRESHOLD" default:"3" range:"[1,100]"`
	// ExcludeEndpoints 是无条件跳过的端点（逗号分隔），给用户一个
	// 不必等自适应收敛的逃生口。
	ExcludeEndpoints []string `env:"EXCLUDE_ENDPOINTS"`
	// EndpointTimeouts 是「端点名=时长」的覆盖列表，形如
	// "wifi_list=20s,lan_info=8s"。取值优先于 profile 内置建议。
	EndpointTimeouts []string `env:"ENDPOINT_TIMEOUTS"`

	// ---- 采集 ----
	CollectMode     string        `env:"COLLECT_MODE"     default:"pull" options:"pull,poll"`
	CollectInterval time.Duration `env:"COLLECT_INTERVAL" default:"30s" range:"[1s,10m]"`
	ScrapeTimeout   time.Duration `env:"SCRAPE_TIMEOUT"   default:"15s" range:"(0s,5m]"`

	// Routers 是实际生效的路由器列表，由 loadRouters 从 ROUTERS 环境变量 /
	// config.json 的 routers 数组 / 单路由器字段三处装配。
	Routers []Router `json:"-"`

	// ConfigFile 记录实际生效的配置文件路径，仅用于日志展示。
	ConfigFile string `json:"-"`
}

// DefaultConfigFile 是未显式指定配置文件时尝试读取的路径。
const DefaultConfigFile = "config.json"

// Overrides 是命令行 flag 的覆盖集合。空字段表示「不覆盖」。
//
// 独立成一个结构体而不是四个散落的字符串参数：flag 与环境变量一一对应，
// 以后再加一个 flag 时不必再改一遍函数签名与调用点。
type Overrides struct {
	MetricsPath string
	Namespace   string
	CollectMode string
	RouterHost  string
}

// Apply 应用命令行覆盖并重新做规范化与校验。
//
// 覆盖之后必须重新跑一遍完整校验（含 range / options tag）：flag 与环境变量
// 一样可能给出非法值（-collect-mode push、-web.telemetry-path 写成空串）。
// 只跑 validate() 是不够的——枚举与范围是 core/config 的 tag 校验负责的，
// 漏掉它就会让 -collect-mode push 这种值一路畅通无阻地进到采集链路里。
func (c *Config) Apply(o Overrides) error {
	if o.MetricsPath != "" {
		c.MetricsPath = o.MetricsPath
	}
	if o.Namespace != "" {
		c.Namespace = o.Namespace
	}
	if o.CollectMode != "" {
		c.CollectMode = o.CollectMode
	}
	if o.RouterHost != "" {
		c.RouterHost = o.RouterHost
	}
	c.normalize()
	if err := config.ValidateTags(c); err != nil {
		return oerr.Wrap(err, "配置校验失败")
	}
	return c.validate()
}

// configJSON 是 Config 的 JSON 形态：四个时长字段用字符串表示。
//
// 为什么不直接给 Config 的 time.Duration 字段加 json tag：json.Unmarshal
// 不认 time.Duration 的字符串形式，配置文件里写 "routerTimeout": "3s" 会报
// `cannot unmarshal string into Go struct field ... of type time.Duration`。
//
// 也没有改用自定义 Duration 类型：github.com/oekit/core/config 是按
// reflect.TypeOf(time.Duration(0)) 识别时长的，自定义类型过不了它的
// default/env/range 解析（会报 `default "5s" is not a valid int`）。
type configJSON struct {
	IP       string `json:"ip"`
	Password string `json:"password"`
	Port     int    `json:"port"`
	Host     string `json:"host"`

	RouterTimeout   string `json:"routerTimeout"`
	ShutdownTimeout string `json:"shutdownTimeout"`
	CollectInterval string `json:"collectInterval"`
	ScrapeTimeout   string `json:"scrapeTimeout"`
}

// UnmarshalJSON 实现带字符串时长的配置解析。
func (c *Config) UnmarshalJSON(b []byte) error {
	var raw configJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.RouterIP = raw.IP
	c.RouterPassword = raw.Password
	c.Port = raw.Port
	c.RouterHost = raw.Host

	var err error
	if c.RouterTimeout, err = parseJSONDuration(raw.RouterTimeout); err != nil {
		return oerr.Wrap(err, "routerTimeout 解析失败")
	}
	if c.ShutdownTimeout, err = parseJSONDuration(raw.ShutdownTimeout); err != nil {
		return oerr.Wrap(err, "shutdownTimeout 解析失败")
	}
	if c.CollectInterval, err = parseJSONDuration(raw.CollectInterval); err != nil {
		return oerr.Wrap(err, "collectInterval 解析失败")
	}
	if c.ScrapeTimeout, err = parseJSONDuration(raw.ScrapeTimeout); err != nil {
		return oerr.Wrap(err, "scrapeTimeout 解析失败")
	}
	return nil
}

// parseJSONDuration 解析配置里的时长；空串返回 0（走 default）。
func parseJSONDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(v), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("无法解析时长 %q（示例：\"5s\"、\"1m30s\"）", s)
	}
	return d, nil
}

// Load 按 环境变量 > config.json > default 的顺序装配配置。
//
// 环境变量优先于配置文件，是沿用旧版行为：旧实现里
//
//	if routerIp != "" && routerPassword != "" { 用环境变量 } else { 读 config.json }
//
// 即配了环境变量就完全不看文件。把这个优先级反过来会让已经按 12-factor
// 方式部署的人（Helm/docker-compose 全靠 env）突然读到文件里的旧值。
//
// path 为空时回退到 DefaultConfigFile；文件不存在不是错误（此时完全依赖
// env 与 default），但文件存在却解析失败是错误——静默忽略一个写坏的配置文件
// 会让 exporter 用默认端口 9001 + 空密码启动，故障现场极其难查。
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigFile
	}

	cfg := &Config{}
	raw, err := loadFile(path, cfg)
	if err != nil {
		return nil, err
	}
	if raw != nil {
		cfg.ConfigFile = path
	}

	// 环境变量单独解析成一层覆盖，再盖到配置文件之上。
	//
	// 不能直接对 cfg 调 ApplyEnv：core/config 的 ApplyEnv 只填「当前为零值」
	// 的字段（面向 yaml → env → default 的接线顺序），而这里需要的是 env
	// 无条件压过文件值。OverlayMerge 恰好是「src 的非零值覆盖 dst」。
	envCfg := &Config{}
	if err := config.ApplyEnv(envCfg); err != nil {
		return nil, oerr.Wrap(err, "读取环境变量失败")
	}
	config.OverlayMerge(cfg, envCfg)

	if err := config.FillDefaults(cfg); err != nil {
		return nil, oerr.Wrap(err, "填充默认值失败")
	}
	if err := config.ValidateTags(cfg); err != nil {
		return nil, oerr.Wrap(err, "配置校验失败")
	}
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := loadRouters(cfg, raw); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadFile 读取 JSON 配置文件。返回 loaded 表示文件是否真实存在。
// loadFile 读取并解析配置文件，返回原始字节。
//
// 原始字节要回传给 loadRouters：单路由器的 Unmarshal 会把 "routers" 数组
// 当作未知字段丢掉，只有原始字节才能再解析一次多路由器写法。
func loadFile(path string, cfg *Config) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, oerr.Wrap(err, "读取配置文件失败", "path", path)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return data, oerr.Wrap(err, "解析配置文件失败", "path", path)
	}
	return data, nil
}

// normalize 统一规范化字符串字段。
//
// 刻意不做任何「顺手修正」：非法取值留给 validate 报错，而不是在这里
// 被悄悄改成另一个合法值。一个值从 70000 变成 65535，排查起来比直接
// 启动失败困难得多。
func (c *Config) normalize() {
	// 指标路径统一成前导斜杠、无尾斜杠：/metrics、metrics/ → /metrics
	c.MetricsPath = "/" + strings.Trim(c.MetricsPath, "/")
	if c.MetricsPath == "/" {
		c.MetricsPath = "/metrics"
	}
	c.RouterIP = strings.TrimSpace(c.RouterIP)
	c.RouterHost = strings.TrimSpace(c.RouterHost)
	c.Namespace = strings.TrimSpace(c.Namespace)
	c.CollectMode = strings.ToLower(strings.TrimSpace(c.CollectMode))
}

// validate 执行业务校验——core/config 的 tag 校验只覆盖范围与枚举，
// 这里补它管不到的部分（namespace 非空、profile 名存在）。
//
// 路由器本身的必填校验在 loadRouters：只有那里才分得清
// 「单路由器字段」与「routers 数组」两种写法该分别要求什么。
func (c *Config) validate() error {
	if c.Namespace == "" {
		return oerr.NewBadRequestError("指标 namespace 不能为空")
	}
	// 这里只校验名字存在性，不做端点能力检查——那是 profile 包的事。
	if c.RouterProfile != "auto" {
		if _, err := profile.Resolve(c.RouterProfile); err != nil {
			return oerr.Wrap(err, "ROUTER_PROFILE 配置无效")
		}
	}
	return nil
}

// ParsedEndpointTimeouts 把 "wifi_list=20s,lan_info=8s" 解析成 map。
//
// 刻意保留原始字符串而不是直接存 map：env 变量只能是字符串，
// 而解析失败时给出明确的哪一段写错了，比让 map 悄悄少一项好排查。
func (c *Config) ParsedEndpointTimeouts() (map[string]time.Duration, error) {
	return parseEndpointTimeouts(c.EndpointTimeouts, "")
}

// parseEndpointTimeouts 解析 "端点名=时长" 列表。
//
// owner 只用于错误信息指明是哪台配错了——多机场景下这个很有用，
// 否则用户看到「第 3 项无法解析」根本不知道是哪台路由器。
func parseEndpointTimeouts(items []string, owner string) (map[string]time.Duration, error) {
	if len(items) == 0 {
		return nil, nil
	}
	where := ""
	if owner != "" {
		where = "（路由器 " + owner + "）"
	}

	out := make(map[string]time.Duration, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			return nil, oerr.NewBadRequestError(fmt.Sprintf(
				"ENDPOINT_TIMEOUTS%s 项 %q 格式应为 端点名=时长（如 wifi_list=20s）", where, item))
		}
		name = strings.TrimSpace(name)
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || d <= 0 {
			return nil, oerr.NewBadRequestError(fmt.Sprintf(
				"ENDPOINT_TIMEOUTS%s 中 %s 的时长 %q 无法解析", where, name, value))
		}
		out[name] = d
	}
	return out, nil
}

// RouterBaseURL 返回路由器的根地址（不含尾部斜杠）。
func (c *Config) RouterBaseURL() string {
	host := strings.TrimSpace(c.RouterIP)
	// 允许直接写 http://192.168.31.1，省掉一层心智负担。
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	// "192.168.31.1" 与 "192.168.31.1:8080" 都原样透传；只有 IPv6 需要补方括号。
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "http://" + net.JoinHostPort(host, "80")
	}
	return "http://" + host
}

// String 实现 fmt.Stringer，输出前抹掉密码。
//
// 密码泄漏不是假设——旧版启动日志直接打印明文密码，一旦日志被采集或外发
// 相当于把路由器管理权限公开。Config 被以 %v / 结构化 kv 打印时一律走这里。
func (c *Config) String() string {
	type safeConfig Config // 避免递归调用 String
	clone := safeConfig(*c)
	clone.RouterPassword = redact(clone.RouterPassword)
	return fmt.Sprintf("%+v", clone)
}

// redact 把密码替换成固定掩码，不泄漏长度。
func redact(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// ValidateRouterHost 单独校验 host 标签字符，避免非法标签名污染指标。
func ValidateRouterHost(host string) error {
	if host == "" {
		return oerr.NewBadRequestError("host 标签不能为空")
	}
	for _, r := range host {
		if r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\'' || r == '\\' {
			return oerr.NewBadRequestError(fmt.Sprintf("host 标签含非法字符 %q", r))
		}
	}
	return nil
}
