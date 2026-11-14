package sqlite

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

func sha256Sum(value []byte) [32]byte { return sha256.Sum256(value) }
func hexEncode(value [32]byte) string { return hex.EncodeToString(value[:]) }
func timeNowUnix() int64 { return time.Now().Unix() }
var _ = strings.Builder{}
