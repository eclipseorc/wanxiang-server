// scrcpy.go 提供远程控制所需的 ADB 配对与连接接口。
//
// 配对与连接是无状态的 adb 操作（幂等），因此放在 admin-server 上，
// 复用现有的管理员认证（requireAdmin）。真正的视频/控制会话由
// scrcpy-server（:9004）的 WS 端点处理，两者不共享状态。
package adminapi

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
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
	// 连接成功后自动切固定端口 5555，使设备监听稳定的 tcpip 端口，供后续
	// 掉线自动重连使用。切换失败不阻断连接（仍可用动态端口控制），仅记录日志。
	if err := r.scrcpy.Tcpip(req.Context(), fmt.Sprintf("%s:%d", body.Host, body.Port), 5555); err != nil {
		log.Printf("scrcpy auto tcpip failed host=%s: %v", body.Host, err)
	}
	_ = r.svc.Store.Audit(u.ID, "scrcpy-connect", "", body.Host)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleScrcpyStatus 查询设备在 adb 中的真实在线状态（是否已完成配对+连接）。
// 前端据此决定打开抽屉时手风琴初始展开到哪一步，避免刷新后重复配对/连接。
func (r *Router) handleScrcpyStatus(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	host := strings.TrimSpace(req.URL.Query().Get("host"))
	portStr := req.URL.Query().Get("port")
	if host == "" || portStr == "" {
		fail(w, http.StatusBadRequest, "host and port are required")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		fail(w, http.StatusBadRequest, "invalid port")
		return
	}
	serial := fmt.Sprintf("%s:%d", host, port)
	online, err := r.scrcpy.IsDeviceOnline(req.Context(), serial)
	if err != nil {
		fail(w, http.StatusInternalServerError, "status failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"online": online})
}

// handleScrcpyUsbDevices 返回当前 adb 中通过 USB 有线连接的设备序列号列表。
// 供「传统模式」切 tcpip 时前端下拉选择，免去用户手填序列号。
func (r *Router) handleScrcpyUsbDevices(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	serials, err := r.scrcpy.UsbSerials(req.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "list usb devices failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"serials": serials})
}

// handleScrcpyTcpip 把一台 USB 连接的设备切换到 TCP 监听模式（adb tcpip <port>）。
// 用于没有「无线调试」菜单的 Android 11 及更早设备：USB 连一次切 tcpip 后，
// 即可拔线用 ip:port 无线连接。serial 是设备序列号（非 ip:port）。
func (r *Router) handleScrcpyTcpip(w http.ResponseWriter, req *http.Request, u sqlite.User) {
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
		Serial string `json:"serial"`
		Port   int    `json:"port"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Serial = strings.TrimSpace(body.Serial)
	if body.Serial == "" {
		fail(w, http.StatusBadRequest, "serial is required")
		return
	}
	if body.Port <= 0 {
		body.Port = 5555 // 传统 adb tcpip 默认端口
	}
	if err := r.scrcpy.Tcpip(req.Context(), body.Serial, body.Port); err != nil {
		fail(w, http.StatusBadRequest, "tcpip failed: "+err.Error())
		return
	}
	_ = r.svc.Store.Audit(u.ID, "scrcpy-tcpip", "", body.Serial)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "port": body.Port})
}
