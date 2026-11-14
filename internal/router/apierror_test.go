package router

import (
	stderrors "errors"
	"testing"
)

// TestAPIErrorFromBody 覆盖错误信封的识别。
//
// 这里的每一条都对应真机实测过的形态：RA70 固件 1.0.140 与 R3P 固件 2.16.29
// 在 stok 失效时回的都是 HTTP 200 + {"code":401,"msg":"Invalid token"}。
func TestAPIErrorFromBody(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantStokBad   bool
		wantAPIFailed bool
	}{
		{"stok 失效（真机形态）", `{"code":401,"msg":"Invalid token"}`, true, false},
		{"403 信封", `{"code":403,"msg":"invalid token"}`, true, false},
		{"普通错误", `{"code":-1,"msg":"no such api"}`, false, true},
		{"正常信封", `{"code":0,"msg":"succ"}`, false, false},
		{"没有 code 字段", `{"hardware":{"mac":"AA:BB:CC:DD:EE:FF"}}`, false, false},
		{"code 不是第一个键", `{"bsd":"miwifi","code":401,"msg":"Invalid token"}`, true, false},
		{"非 JSON", `<html>login page</html>`, false, false},
		{"空响应", ``, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := apiErrorFromBody([]byte(tc.body))
			if got := stderrors.Is(err, ErrStokInvalid); got != tc.wantStokBad {
				t.Errorf("ErrStokInvalid = %v，期望 %v（err=%v）", got, tc.wantStokBad, err)
			}
			if got := stderrors.Is(err, ErrAPIError); got != tc.wantAPIFailed {
				t.Errorf("ErrAPIError = %v，期望 %v（err=%v）", got, tc.wantAPIFailed, err)
			}
		})
	}
}

// TestLooksLikeErrorEnvelope 覆盖前缀预判：真机的错误信封固定以 {"code": 开头。
func TestLooksLikeErrorEnvelope(t *testing.T) {
	if !looksLikeErrorEnvelope([]byte(`{"code":401,"msg":"Invalid token"}`)) {
		t.Error("真机形态的错误信封应被前缀预判命中")
	}
	if looksLikeErrorEnvelope([]byte(`{"hardware":{"mac":"AA"}}`)) {
		t.Error("正常载荷不应被误判为错误信封")
	}
}

// TestIsEmptyTarget 覆盖「载荷解出来是空的」这一判定。
func TestIsEmptyTarget(t *testing.T) {
	type payload struct {
		Name string
		List []int
	}
	if !isEmptyTarget(&payload{}) {
		t.Error("零值结构体应判定为空")
	}
	if isEmptyTarget(&payload{Name: "x"}) {
		t.Error("非零值结构体不应判定为空")
	}
	if isEmptyTarget(nil) {
		t.Error("nil 不应判定为空目标")
	}
}
