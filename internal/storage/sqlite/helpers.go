package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// sha256Sum 对敏感令牌等输入计算 SHA-256 摘要，避免明文直接持久化。
func sha256Sum(value []byte) [32]byte { return sha256.Sum256(value) }

// hexEncode 将固定长度摘要编码成便于存入数据库的十六进制字符串。
func hexEncode(value [32]byte) string { return hex.EncodeToString(value[:]) }

// timeNowUnix 返回当前 Unix 时间戳，供数据库记录创建时间使用。
func timeNowUnix() int64 { return time.Now().Unix() }

// 保留 strings 的兼容引用；现有代码中的字符串处理会逐步集中到 Store。
var _ = strings.Builder{}
