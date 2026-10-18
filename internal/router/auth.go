package router

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	oerr "github.com/oekit/pkg/errors"

	"github.com/helloworlde/miwifi-exporter/internal/profile"
)

var (
	// ErrLoginFailed 表示登录流程走到了最后但没拿到 stok：密码错误、固件改版等。
	ErrLoginFailed = stderrors.New("miwifi: 登录失败")
	// ErrStokInvalid 表示 stok 过期或被路由器作废，需要重新登录。
	ErrStokInvalid = stderrors.New("miwifi: stok 无效")
)

// session 是一次成功登录的产物。
type session struct {
	stok    string
	loginAt time.Time
}

// doLogin 走完 MiWiFi 的三步登录：取 key/deviceId → 取 init_info → 提交口令换 stok。
//
// 调用方 Client.refreshStok 已持有 loginMu，这里不需要再自己防并发。
//
// 与旧实现的关键差别在于「不再假设正则一定能匹配」：旧代码直接对
// FindStringSubmatch 的结果取下标 [1]，固件改版或页面结构变化时会
// index out of range 直接 panic，而这段代码运行在 Prometheus 的
// scrape 路径上，panic 会打断整个采集。这里全部返回 error。
func (c *Client) doLogin(ctx context.Context) (session, error) {
	webBody, err := c.getRaw(ctx, "/cgi-bin/luci/web")
	if err != nil {
		return session{}, oerr.Wrap(err, "获取登录页失败")
	}

	// 登录页解析规则由 profile 决定——这是固件版本差异最大的一环，
	// 具体 parser 见 internal/profile/login.go。
	page, err := c.profile.Profile.LoginPageParser().Parse(string(webBody))
	if err != nil {
		return session{}, oerr.Wrap(err, "解析登录页失败")
	}

	var initInfo InitInfo
	if err := c.getJSON(ctx, "/cgi-bin/luci/api/xqsystem/init_info", &initInfo, c.http.Timeout); err != nil {
		return session{}, oerr.Wrap(err, "获取 init_info 失败")
	}

	nonce := fmt.Sprintf("0_%s_%d_962", page.DeviceID, time.Now().Unix())

	// 散列算法由固件上报的 newEncryptMode 决定，映射关系见 profile.builtin。
	hasher, err := c.profile.Profile.Hasher(initInfo.NewEncryptMode)
	if err != nil {
		return session{}, oerr.Wrap(err, "选择口令散列算法失败")
	}
	password := hasher(c.password, page.Key, nonce)

	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", password)
	form.Set("logtype", "2")
	form.Set("nonce", nonce)

	body, err := c.postForm(ctx, "/cgi-bin/luci/api/xqsystem/login", form)
	if err != nil {
		return session{}, oerr.Wrap(err, "提交登录请求失败")
	}

	var resp struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return session{}, oerr.Wrap(err, "解析登录响应失败")
	}
	if resp.URL == "" && resp.Token == "" {
		return session{}, oerr.Wrap(ErrLoginFailed, "登录响应中没有 url/token", "code", resp.Code, "msg", resp.Msg)
	}

	// 登录成功但没带 code=0 的老固件，靠 url/token 存在性判断即可。
	token := resp.Token
	path := resp.URL
	if token == "" && path != "" {
		token, err = profile.ExtractStok(path)
		if err != nil {
			return session{}, oerr.Wrap(err, "解析登录响应 stok 失败")
		}
	}
	if token == "" {
		return session{}, oerr.Wrap(ErrLoginFailed, "登录响应缺少 stok")
	}

	c.logger.Info("路由器登录成功",
		"hardware", initInfo.Hardware,
		"rom_version", initInfo.RomVersion,
		"encrypt_mode", initInfo.NewEncryptMode,
		"profile", c.profile.Name())
	return session{stok: token, loginAt: time.Now()}, nil
}

// getRaw 发一个 GET 并返回原始响应体。
func (c *Client) getRaw(ctx context.Context, path string) ([]byte, error) {
	return c.request(ctx, http.MethodGet, path, nil, c.http.Timeout)
}

// postForm 以 application/x-www-form-urlencoded 提交并返回原始响应体。
func (c *Client) postForm(ctx context.Context, path string, form url.Values) ([]byte, error) {
	return c.request(ctx, http.MethodPost, path, strings.NewReader(form.Encode()), c.http.Timeout)
}

// getJSON 发一个 GET 并把响应解码到 out。
func (c *Client) getJSON(ctx context.Context, path string, out any, timeout time.Duration) error {
	body, err := c.request(ctx, http.MethodGet, path, nil, timeout)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return oerr.Wrap(err, "解析 JSON 失败", "path", path)
	}
	return nil
}

// truncateBody 给错误信息里的响应体加长度上限，避免把整页 HTML 灌进日志。
func truncateBody(b []byte) string {
	const maxRunes = 200
	runes := []rune(string(b))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
		return string(runes) + "…(truncated)"
	}
	return string(runes)
}
