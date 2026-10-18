package profile

import (
	"crypto/sha1" // #nosec G505 // 散列选择由固件的 newEncryptMode 决定，是协议要求而非选型
	"crypto/sha256"
	"encoding/hex"
)

func hashSHA1(s string) string {
	sum := sha1.Sum([]byte(s)) // #nosec G401 // 同上：MiWiFi 登录协议规定
	return hex.EncodeToString(sum[:])
}

func hashSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
