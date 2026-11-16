package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// sha256Sum 对敏感令牌等输入计算 SHA-256 摘要，避免明文直接持久化。
func sha256Sum(value []byte) [32]byte { return sha256.Sum256(value) }

// hexEncode 将固定长度摘要编码成便于存入数据库的十六进制字符串。
func hexEncode(value [32]byte) string { return hex.EncodeToString(value[:]) }

// timeNowStamp 返回与 SQLite CURRENT_TIMESTAMP 一致的时间字符串。
// users.created_at 是 TEXT 列，写入 Unix 整数会被转成不可读的数字字符串。
func timeNowStamp() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }
