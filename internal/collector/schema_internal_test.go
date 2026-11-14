package collector

import "testing"

// TestSchemaFingerprint 差异指纹必须稳定、且对任何变化敏感。
//
// 指纹决定「差异变了没有」：变了才记录日志。
// 指纹要是把顺序也算进去，差异集合每轮顺序一变就会重复报一遍；
// 指纹要是分不清成因，配置差异与固件差异就会互相覆盖。
func TestSchemaFingerprint(t *testing.T) {
	a := schemaDiff{Kind: kindFirmware, MissingHost: "a", Endpoint: "wifi_list"}
	b := schemaDiff{Kind: kindConfig, MissingHost: "b", Endpoint: "lan_info"}
	c := schemaDiff{Kind: kindFault, MissingHost: "a", Endpoint: "wan_info"}

	fpAB := schemaFingerprint([]schemaDiff{a, b})
	fpBA := schemaFingerprint([]schemaDiff{b, a})
	if fpAB != fpBA {
		t.Errorf("指纹不应受顺序影响: %q vs %q", fpAB, fpBA)
	}

	// 少一个差异 → 指纹必须变（否则差异消失时不会被记录）。
	if fpAB == schemaFingerprint([]schemaDiff{a}) {
		t.Error("差异减少时指纹应变化")
	}
	// 多一个差异 → 指纹必须变。
	if fpAB == schemaFingerprint([]schemaDiff{a, b, c}) {
		t.Error("差异增加时指纹应变化")
	}
	// 成因变了 → 指纹必须变（配置差异与固件差异要能被区分开）。
	// 同样的端点、同样的机器，只是停采原因变了。
	c2 := schemaDiff{Kind: kindFirmware, MissingHost: "b", Endpoint: "lan_info"}
	if schemaFingerprint([]schemaDiff{a, b}) == schemaFingerprint([]schemaDiff{a, c2}) {
		t.Error("成因不同时指纹应变化")
	}
}
