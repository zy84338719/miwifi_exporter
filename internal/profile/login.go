package profile

// 本文件集中管理**登录相关的固件差异**——这是整个 exporter 里最脆弱的一环。
//
// 登录页是一段由路由器 Web 服务动态拼出来的 JavaScript，厂商每改一次
// 前端就会变动一次结构。反过来说：只要哪天登录失败而密码是对的，
// 十有八九就是这里要加一个 parser。
//
// 历史事故（本次重构修复）：旧实现对正则结果直接取下标 [1]，
// 页面结构一变就 index out of range panic，而这个代码运行在 scrape 路径上。
// 所以下面每个 parser 都必须返回 error，绝不能假设一定能匹配。

import (
	"fmt"
	"regexp"
	"strings"
)

// sha1Hasher / sha256Hasher 是两种已知的口令散列算法。
func sha1Hasher(password, key, nonce string) string {
	a := hashSHA1(password + key)
	return hashSHA1(nonce + a)
}

func sha256Hasher(password, key, nonce string) string {
	a := hashSHA256(password + key)
	return hashSHA256(nonce + a)
}

// LuCIParser 解析标准 LuCI 登录页。
//
// 覆盖绝大多数型号，页面里长这样：
//
//	var encrypt = { key: '0123456789abcdef', };
//	var deviceId = 'ABCDEF0123';
type LuCIParser struct {
	keyRe      *regexp.Regexp
	deviceRe   *regexp.Regexp
	descripton string
}

// NewLuCIParser 用给定正则构造解析器；正则非法时返回错误。
func NewLuCIParser(keyRe, deviceRe *regexp.Regexp, description string) (*LuCIParser, error) {
	if keyRe == nil || deviceRe == nil {
		return nil, fmt.Errorf("profile: LuCIParser 的正则不能为 nil")
	}
	if description == "" {
		description = "LuCI 标准登录页"
	}
	return &LuCIParser{keyRe: keyRe, deviceRe: deviceRe, descripton: description}, nil
}

// Parse 实现 LoginPageParser。
func (p *LuCIParser) Parse(html string) (LoginPageInfo, error) {
	// 先把 \r \n \t 去掉：不同固件/不同 web server 的换行策略不一样，
	// 但 JS 语义不依赖这些空白，抹平可以少写一堆 (?s) 之类的修饰符。
	cleaned := strings.NewReplacer("\r", "", "\n", "", "\t", "").Replace(html)

	key, err := firstMatch(p.keyRe, cleaned, "登录页加密 key")
	if err != nil {
		return LoginPageInfo{}, err
	}
	deviceID, err := firstMatch(p.deviceRe, cleaned, "登录页 deviceId")
	if err != nil {
		return LoginPageInfo{}, err
	}
	return LoginPageInfo{Key: key, DeviceID: deviceID}, nil
}

// Describe 实现 LoginPageParser。
func (p *LuCIParser) Describe() string { return p.descripton }

// DefaultLuCIParser 是标准 LuCI 页面解析器，也是所有内置 profile 的兜底。
//
// 这两条正则是从重构前实现原样搬过来的。换固件时若匹配不到，
// 正确做法是在这里旁边再加一个 parser，而不是就地改正则——
// 直接改正则会让「哪些型号用哪个 parser」这条信息丢失。
func DefaultLuCIParser() *LuCIParser {
	p, err := NewLuCIParser(
		regexp.MustCompile(`key:.*?'(.*?)',`),
		regexp.MustCompile(`deviceId = '(.*?)';`),
		"LuCI 标准登录页（key/deviceId 正则）",
	)
	if err != nil {
		// 正则字面量编译失败只可能是代码写错了，属于启动期必须暴露的问题。
		panic("profile: 内置 LuCIParser 正则非法: " + err.Error())
	}
	return p
}

// firstMatch 取第一个捕获组；未匹配时返回可读错误而不是 panic。
func firstMatch(re *regexp.Regexp, s, what string) (string, error) {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 || m[1] == "" {
		return "", fmt.Errorf("未能从登录页解析 %s（固件页面结构可能已变化，需要新增 LoginPageParser）", what)
	}
	return m[1], nil
}

// StokRe 从登录响应返回的 url 中抠出 stok。
//
// 这条不属于「登录页解析」而属于「登录响应解析」，两者分开是因为
// 变化的原因不同：页面结构随前端变，响应结构随后端 API 版本变。
var StokRe = regexp.MustCompile(`;stok=(.*?)/`)

// ExtractStok 从登录响应里取 stok。
func ExtractStok(url string) (string, error) {
	m := StokRe.FindStringSubmatch(url)
	if len(m) < 2 || m[1] == "" {
		return "", fmt.Errorf("未能从登录响应的 url %q 中解析 stok", redact(url))
	}
	return m[1], nil
}

// redact 限制可能含凭据的字符串长度，避免把整段响应灌进日志。
func redact(s string) string {
	const max = 80
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
