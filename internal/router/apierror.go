package router

import (
	"bytes"
	"encoding/json"
	stderrors "errors"

	oerr "github.com/oekit/pkg/errors"
)

// ErrAPIError 表示路由器用 HTTP 200 回了一个错误信封。
//
// 与 ErrHTTPStatus 的区别：那个是状态码本身就不是 2xx，这个是状态码完全正常、
// 但 body 里带着 {"code":<非0>,"msg":"..."}。真机实测（RA70 固件 1.0.140）：
// stok 失效时回的正是后者——HTTP 200 + {"code":401,"msg":"Invalid token"}。
var ErrAPIError = stderrors.New("miwifi: 路由器返回错误信封")

// apiEnvelope 是 MiWiFi 各 JSON 接口共有的外层信封。
type apiEnvelope struct {
	Code *int   `json:"code"`
	Msg  string `json:"msg"`
}

// apiErrorFromBody 识别「HTTP 200 但 body 是错误信封」的响应。
//
// 为什么必须识别：小米路由器的 stok 失效时**不**回 401/403，而是回 200 +
// {"code":401,"msg":"Invalid token"}。只按 HTTP 状态码判断的话，这个响应会被
// 当成一次成功采集：响应体被解析成全零结构且不报任何错，于是
//
//   - mac / sn / platform / version 变成空串
//   - uptime 变成 0
//   - 设备级序列整片消失
//   - 端点被记成「成功」，永远不会触发重新登录
//
// 结果就是「指标大面积消失但一切看起来正常」。实测踩过：2026-11-14 在
// 10.10.10.31 上，另一台实例登录顶掉了会话，线上实例的 144 条序列
// 静默消失，直到重启容器才恢复。
//
// 识别出来之后，401 会被归一成 ErrStokInvalid，正好落进 call() 已有的
// 「换新 stok 再试一次」自愈路径——那条路径本来就该接住这种情况，
// 只是过去永远等不到触发条件。
func apiErrorFromBody(body []byte) error {
	code, msg, ok := apiEnvelopeOf(body)
	if !ok || code == 0 {
		return nil
	}
	if code == 401 || code == 403 {
		return oerr.Wrap(ErrStokInvalid, "路由器判定 stok 无效", "code", code, "msg", msg)
	}
	return oerr.Wrap(ErrAPIError, "路由器返回错误信封", "code", code, "msg", msg)
}

// apiEnvelopeOf 取出顶层信封里的 code 与 msg。
//
// 只关心顶层信封：内层对象里也可能出现名为 code 的字段，那不是接口级错误。
func apiEnvelopeOf(body []byte) (int, string, bool) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, "", false
	}
	var env apiEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return 0, "", false
	}
	if env.Code == nil {
		return 0, env.Msg, false
	}
	return *env.Code, env.Msg, true
}

// looksLikeErrorEnvelope 判断响应体是否以错误信封的形态开头。
//
// 真机实测错误信封固定以 {"code": 开头（RA70 固件 1.0.140、R3P 固件 2.16.29
// 均如此）。用它做一次零成本的预判：命中就直接走错误分支，不必先解析载荷。
func looksLikeErrorEnvelope(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("{\"code\":"))
}
