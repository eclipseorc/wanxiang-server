// scrcpy.go 提供远程控制所需的 ADB 配对与连接接口。
//
// 配对与连接是无状态的 adb 操作（幂等），因此放在 admin-server 上，
// 复用现有的管理员认证（requireAdmin）。真正的视频/控制会话由
// scrcpy-server（:9004）的 WS 端点处理，两者不共享状态。
package adminapi

import (
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// handleScrcpyPair 处理无线调试配对：接收设备 IP、配对端口与 6 位配对码。
func (r *Router) handleScrcpyPair(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	var body struct {
		Host     string `json:"host"`
		PairPort int    `json:"pairPort"`
		Code     string `json:"code"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Host = strings.TrimSpace(body.Host)
	body.Code = strings.TrimSpace(body.Code)
	if body.Host == "" || body.PairPort <= 0 || body.Code == "" {
		fail(w, http.StatusBadRequest, "host, pairPort and code are required")
		return
	}
	if err := r.scrcpy.Pair(req.Context(), body.Host, body.PairPort, body.Code); err != nil {
		fail(w, http.StatusBadRequest, "pair failed: "+err.Error())
		return
	}
	_ = r.svc.Store.Audit(u.ID, "scrcpy-pair", "", body.Host)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleScrcpyConnect 建立无线调试连接：接收设备 IP 与连接端口。
func (r *Router) handleScrcpyConnect(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Host = strings.TrimSpace(body.Host)
	if body.Host == "" || body.Port <= 0 {
		fail(w, http.StatusBadRequest, "host and port are required")
		return
	}
	if err := r.scrcpy.Connect(req.Context(), body.Host, body.Port); err != nil {
		fail(w, http.StatusBadRequest, "connect failed: "+err.Error())
		return
	}
	_ = r.svc.Store.Audit(u.ID, "scrcpy-connect", "", body.Host)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
