package notifywatcher

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex 计算字符串的 SHA-256 十六进制（指纹去重键）。
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
