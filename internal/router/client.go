package router

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/oekit/concurrency/breaker"
	"github.com/oekit/concurrency/retry"
	"github.com/oekit/logx"
	oerr "github.com/oekit/pkg/errors"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
)

// Options 是构造 Client 所需的参数。
type Options struct {
	// BaseURL 是路由器根地址，形如 http://192.168.31.1。
	BaseURL string
	// Password 是路由器后台密码。
	Password string
	// Timeout 是单次 HTTP 请求的超时。
	Timeout time.Duration
	// StokTTL 是 stok 的最长使用时长；超过即主动重新登录。
	//
	// 主动过期是刻意的：MiWiFi 的 stok 由路由器自行作废，客户端无法精确知道
	// 剩余寿命。与其等到某次 scrape 撞上 403 才被动重登，不如在 TTL 边界
	// 提前换掉——反正登录只要两个 RTT。
	StokTTL time.Duration
	// Retry 覆盖默认的重试参数，零值时用 DefaultRetry。
	Retry retry.Config
	// ProfileName 指定固件 profile；空或 "auto" 表示交给自适应探测。
	// 具体差异见 internal/profile 包。
	ProfileName string
	// EndpointTimeoutThreshold 是「连续超时多少次后停问该端点」，
	// <=0 时用 profile.DefaultTimeoutThreshold。
	EndpointTimeoutThreshold int
	// ExcludeEndpoints 是要无条件跳过的端点名，给用户一个
	// 不必等自适应收敛的逃生口。
	ExcludeEndpoints []string
	// EndpointTimeouts 是「端点名 → 专用超时」的覆盖表，优先于 profile 的
	// 内置建议（env 读作 "wifi_list=20s,lan_info=8s"）。
	EndpointTimeouts map[string]time.Duration
	// Logger 覆盖默认 logger。
	Logger logx.Logger
}

const (
	defaultTimeout = 5 * time.Second
	defaultStokTTL = 30 * time.Minute
)

// DefaultRetry 是内建重试参数：3 次尝试、指数退避 + 抖动。
//
// 抖动是必须的——十个端点并发失败时，没有抖动的固定退避会让它们在同一毫秒
// 一起重试，对已经过载的路由器无异于雪上加霜。
func DefaultRetry() retry.Config {
	return retry.Config{
		Attempts:    3,
		Delay:       200 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		Exponential: true,
		Jitter:      150 * time.Millisecond,
		Retryable:   IsRetryable,
	}
}

// Client 是并发安全的路由器 API 客户端。
//
// 它自身持有 stok 与「每个端点最后一次成功结果」，因此上层即使在路由器
// 局部故障时也能拿到一份可用快照，而不是一串空洞。
type Client struct {
	baseURL  string
	password string
	logger   logx.Logger
	// profile 决定「这台机器上哪些接口存在、登录页怎么解析」。
	profile *profile.Instance

	http    *http.Client
	retry   retry.Config
	breaker breaker.Breaker

	// stokTTLValue 由 Options 归一化而来，构造后不再变化。
	stokTTLValue time.Duration
	// timeoutOverrides 是用户为个别端点指定的超时预算。
	timeoutOverrides map[string]time.Duration

	// stokMu 只保护 stok 与 loginAt 这两个字段。
	stokMu  sync.RWMutex
	stok    string
	loginAt time.Time

	// loginMu 串行化登录。必须互斥而不是靠 singleflight：
	// MiWiFi 在新登录成功后会作废此前的会话，��后登录会把前一个 stok 打成
	// 失效——并发下两个 goroutine 同时登录，结果是先拿到 stok 的那个必然失败。
	// 登录本身只有两个 RTT，串行化的代价可以忽略。
	loginMu sync.Mutex

	// goodMu 保护 lastGood；lastGood 的 value 是 *T（指向快照里的字段）。
	goodMu   sync.Mutex
	lastGood map[string]any

	closeOnce sync.Once
}

// New 构造 Client。BaseURL 与 Password 必填。
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, oerr.NewBadRequestError("BaseURL 不能为空")
	}
	if opts.Password == "" {
		return nil, oerr.NewBadRequestError("路由器密码不能为空")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ttl := opts.StokTTL
	if ttl <= 0 {
		ttl = defaultStokTTL
	}
	rcfg := opts.Retry
	if rcfg.Attempts <= 0 {
		rcfg = DefaultRetry()
	}
	if rcfg.Retryable == nil {
		rcfg.Retryable = IsRetryable
	}
	logger := opts.Logger
	if logger == nil {
		logger = logx.Delegating("router")
	}

	prof, err := profile.SelectWith(opts.ProfileName, opts.EndpointTimeoutThreshold)
	if err != nil {
		return nil, oerr.Wrap(err, "选择固件 profile 失败")
	}
	for _, ep := range opts.ExcludeEndpoints {
		ep = strings.TrimSpace(ep)
		if ep == "" {
			continue
		}
		if !prof.Profile.Supports(ep) {
			return nil, oerr.NewBadRequestError("要排除的端点不在任何 profile 的支持范围内: " + ep)
		}
		prof.Disable(ep)
	}
	if len(opts.ExcludeEndpoints) > 0 {
		logger.Info("按配置排除端点", "endpoints", opts.ExcludeEndpoints)
	}

	// 用 cookiejar 而不是手写 Cookie 头：登录后路由器会下发会话 cookie，
	// 后续 API 调用带上它才不会被判定为未登录。旧实现往 map 里塞了一个
	// 名为 "Cookies" 的自定义头——那不是合法的 HTTP 头，等于没发。
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, oerr.Wrap(err, "创建 cookie jar 失败")
	}

	return &Client{
		baseURL:  strings.TrimSuffix(opts.BaseURL, "/"),
		password: opts.Password,
		logger:   logger,
		profile:  prof,
		http: &http.Client{
			Timeout: timeout,
			Jar:     jar,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   timeout,
				ExpectContinueTimeout: time.Second,
			},
		},
		retry:            rcfg,
		breaker:          breaker.NewBreaker("miwifi-router"),
		stokTTLValue:     ttl,
		timeoutOverrides: opts.EndpointTimeouts,
		lastGood:         make(map[string]any),
	}, nil
}

// IsRetryable 判断错误是否值得重试。
//
// 判定原则：网络层与「可能自愈」的失败重试，「逻辑上必然失败」的立即返回。
// ctx 取消/超时不重试——重试一个已经没人等的请求只是浪费。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// stokFor 返回一个当前可用的 stok，必要时重新登录。
func (c *Client) stokFor(ctx context.Context) (string, error) {
	if stok := c.peekStok(); stok != "" {
		return stok, nil
	}
	return c.refreshStok(ctx)
}

// peekStok 返回未过期且非空的 stok，否则返回空串。
func (c *Client) peekStok() string {
	c.stokMu.RLock()
	defer c.stokMu.RUnlock()
	if c.stok != "" && time.Since(c.loginAt) < c.stokTTLValue {
		return c.stok
	}
	return ""
}

// invalidate 声明某个 stok 已失效。
func (c *Client) invalidate(stok string) {
	c.stokMu.Lock()
	defer c.stokMu.Unlock()
	// 只作废调用方看到的那一个，避免把别的 goroutine 刚换上的新 stok 又清掉。
	if c.stok == stok {
		c.stok = ""
		c.loginAt = time.Time{}
	}
}

// refreshStok 串行化地重新登录。
func (c *Client) refreshStok(ctx context.Context) (string, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()

	// 拿到锁后再确认一次：等锁期间可能已经有别的 goroutine 换好了 stok。
	if stok := c.peekStok(); stok != "" {
		return stok, nil
	}

	sess, err := c.doLogin(ctx)
	if err != nil {
		return "", err
	}

	c.stokMu.Lock()
	c.stok, c.loginAt = sess.stok, sess.loginAt
	stok := c.stok
	c.stokMu.Unlock()
	return stok, nil
}

// endpointTimeout 返回该 API 的专用超时预算。
//
// 取「全局值与专用值中更宽的一个」而不是「专用值优先」：这样用户把
// ROUTER_TIMEOUT 调到 30s 时不会被某个端点的窄预算悄悄压回去——
// 调大全局超时本意就是「我愿意等更久」。
func (c *Client) endpointTimeout(apiPath string) time.Duration {
	base := c.http.Timeout
	if base <= 0 {
		base = defaultTimeout
	}
	for name, p := range apiPathIndex {
		if p != apiPath {
			continue
		}
		if d, ok := c.timeoutOverrides[name]; ok && d > base {
			return d
		}
		if d, ok := c.profile.Profile.TimeoutFor(name); ok && d > base {
			return d
		}
		break
	}
	return base
}

// EndpointTimeoutOf 暴露某个 API 路径实际会用的超时预算，供测试与排障使用。
func (c *Client) EndpointTimeoutOf(apiPath string) time.Duration { return c.endpointTimeout(apiPath) }

// request 执行一次真实的 HTTP 请求并返回响应体。
//
// timeout 单独传而不是只依赖 http.Client.Timeout：后者是全局的，
// 而不同端点的耗时差了两个数量级（0.2s vs 13s），共用一个值必然
// 要么太慢要么太短。客户端级 Timeout 仍保留为兜底上限。
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var respBody []byte

	// 熔断器挂在「路由器整体」这一层：路由器宕机时连续失败会打开熔断，
	// 之后的调用立即返回 ErrCircuitOpen，而不必让十个端点各等一个超时。
	// 非传输层错误（4xx、解析失败）算「可接受」，不计入失败样本——
	// 否则一个返回 404 的端点就能把整个路由器熔断掉。
	err := c.breaker.DoWithAcceptable(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
		if err != nil {
			return oerr.Wrap(err, "构造请求失败")
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return oerr.Wrap(err, "请求路由器失败")
		}
		defer resp.Body.Close()

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return oerr.Wrap(err, "读取响应失败")
		}

		// stok 失效时路由器回 403（部分固件回 401 并重定向到登录页）。
		// 单独识别出来，才能让重试循环在下一次尝试里换上新 stok——
		// 否则会拿着一个必然失败的 stok 白白重试完整个退避序列。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return oerr.Wrap(ErrStokInvalid, "stok 已失效", "status", resp.StatusCode)
		}
		// 404 / 501 是「这台机器上没有这个接口」的明确信号，
		// 与 5xx（临时故障）性质完全不同：前者重试一万次也没用。
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
			return oerr.Wrap(ErrEndpointMissing, "该固件不提供此接口", "status", resp.StatusCode)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return oerr.Wrap(ErrHTTPStatus, "路由器返回非 2xx", "status", resp.StatusCode, "body", truncateBody(data))
		}

		respBody = data
		return nil
	}, func(err error) bool {
		return !isTransportError(err)
	})
	if err != nil {
		return nil, err
	}
	return respBody, nil
}

var (
	// ErrHTTPStatus 表示路由器返回了非 2xx 且不是鉴权失败的状态码。
	ErrHTTPStatus = stderrors.New("miwifi: 路由器返回非 2xx")
	// ErrEndpointMissing 表示该固件压根没有这个接口（HTTP 404/501）。
	// 与 ErrHTTPStatus 分开，是因为它的正确处置是「别再问了」而不是「重试」。
	ErrEndpointMissing = stderrors.New("miwifi: 该固件不提供此接口")
)

// isTransportError 判断错误是否属于「路由器可能暂时不可用」这一类。
//
// 只有传输层错误计入熔断失败样本：4xx、解析失败这类「逻辑上必然失败」
// 的错误如果也计入，一个返回 404 的端点就能把整台路由器熔断掉。
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if stderrors.Is(err, ErrHTTPStatus) ||
		stderrors.Is(err, ErrStokInvalid) ||
		stderrors.Is(err, ErrEndpointMissing) {
		return false
	}
	// *url.Error 实现了 net.Error（Timeout/Temporary），这一条已经覆盖
	// http.Client.Do 返回的全部传输层错误，无需再单独 As *url.Error。
	var netErr net.Error
	return stderrors.As(err, &netErr)
}

// call 调用一个带 stok 的 API 并把结果解码到 out。
//
// stok 失效单独走一条与重试策略无关的自愈路径：拿到新 stok 再试一次。
// Retry.Attempts 表达的是「网络抖动要不要重试」，把它套在「token 过期」
// 上是搞错了语义——用户把 Attempts 调成 1 不代表希望 token 过期后
// 整轮采集全废。
func (c *Client) call(ctx context.Context, path string, out any) error {
	err := c.callWithRetry(ctx, path, out)
	if !stderrors.Is(err, ErrStokInvalid) {
		return err
	}
	return c.callWithRetry(ctx, path, out)
}

// callWithRetry 执行一次「带重试策略」的调用。
func (c *Client) callWithRetry(ctx context.Context, path string, out any) error {
	return retry.Do(ctx, c.retry, func(attempt int) error {
		stok, err := c.stokFor(ctx)
		if err != nil {
			return err
		}

		// 每次尝试前清空目标：json.Unmarshal 是「合并」语义而非「替换」，
		// 上一轮响应里有、本轮没有的字段会残留在结构体里，把两次采集的数据
		// 混成一份既不属于上一轮也不属于这一轮的东西。
		resetTarget(out)

		err = c.getJSON(ctx, fmt.Sprintf("/cgi-bin/luci/;stok=%s/%s", stok, path), out, c.endpointTimeout(path))
		if stderrors.Is(err, ErrStokInvalid) {
			c.invalidate(stok)
		}
		return err
	})
}

// resetTarget 把指针指向的值清零，使随后的解码从零值开始。
func resetTarget(out any) {
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	elem := v.Elem()
	switch elem.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array, reflect.Interface:
		if elem.CanSet() {
			elem.Set(reflect.Zero(elem.Type()))
		}
	}
}

// apiPath 是不带 stok 前缀的 API 路径。
//
// 名字部分复用 profile 包的端点常量，避免「profile 里叫 status、
// client 里叫 pathStatus」这种改名时对不上的漂移。
// apiPathIndex 给出「端点名 → API 路径」的反查表，用于按端点定位超时预算。
var apiPathIndex = map[string]string{
	profile.EPStatus:          pathStatus,
	profile.EPDeviceList:      pathDeviceList,
	profile.EPNewStatus:       pathNewStatus,
	profile.EPDeviceListV2:    pathDeviceListV2,
	profile.EPWanInfo:         pathWanInfo,
	profile.EPWifiDetailAll:   pathWifiDetailAll,
	profile.EPPppoeStatus:     pathPppoeStatus,
	profile.EPLanInfo:         pathLanInfo,
	profile.EPWifiConnDevices: pathWifiConnDevice,
	profile.EPWifiList:        pathWifiList,
}

var (
	pathStatus       = "api/misystem/" + profile.EPStatus
	pathDeviceList   = "api/misystem/" + profile.EPDeviceList
	pathNewStatus    = "api/misystem/" + profile.EPNewStatus
	pathDeviceListV2 = "api/misystem/" + profile.EPDeviceListV2
	pathWanInfo      = "api/xqnetwork/" + profile.EPWanInfo
	pathPppoeStatus  = "api/xqnetwork/" + profile.EPPppoeStatus
	pathLanInfo      = "api/xqnetwork/" + profile.EPLanInfo
	pathWifiList     = "api/xqnetwork/" + profile.EPWifiList

	pathWifiDetailAll  = "api/xqnetwork/" + profile.EPWifiDetailAll
	pathWifiConnDevice = "api/xqnetwork/" + profile.EPWifiConnDevices
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/90.0.4430.72 Safari/537.36"

// Profile 返回本客户端的固件 profile，供上层暴露给指标与日志。
func (c *Client) Profile() *profile.Instance { return c.profile }

// Close 释放底层连接。幂等。
func (c *Client) Close(context.Context) error {
	c.closeOnce.Do(func() {
		if tr, ok := c.http.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	})
	return nil
}
