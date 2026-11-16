package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// contextKey 使用私有类型，避免与其它包放进 context 的键发生碰撞。
type contextKey int

const userKey contextKey = 1

// contextWithUser 把认证后的用户挂到请求上下文，供下游 handler 取用。
// 权限判断仍以服务端数据库为准，不信任客户端传入的角色字段。
func contextWithUser(r *http.Request, u sqlite.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey, u))
}

// userFromContext 取出认证中间件写入的用户；缺失时返回零值，调用方应视为未认证。
func userFromContext(r *http.Request) sqlite.User {
	u, _ := r.Context().Value(userKey).(sqlite.User)
	return u
}

// randomID 生成 16 字节随机值对应的十六进制字符串，用作组等业务主键。
func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
