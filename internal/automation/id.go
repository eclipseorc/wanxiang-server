package automation

import (
	"crypto/rand"
	"encoding/hex"
)

// randHex 生成指定字节数的随机十六进制字符串,用于日志等业务主键。
// 生成失败时返回空串(调用方应容忍,日志主键非关键路径)。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
