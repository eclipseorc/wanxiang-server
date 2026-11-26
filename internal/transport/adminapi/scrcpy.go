// scrcpy.go 提供远程控制所需的 ADB 配对与连接接口。
//
// 配对与连接是无状态的 adb 操作（幂等），因此放在 admin-server 上，
// 复用现有的管理员认证（requireAdmin）。真正的视频/控制会话由
// scrcpy-server（:9004）的 WS 端点处理，两者不共享状态。
package adminapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// friendlyAdbError 把 adb 命令返回的常见英文错误转成对普通用户友好的中文提示。
// 其它未识别的错误原样透传（错误信息本身仍有诊断价值）。
func friendlyAdbError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unauthorized"):
		return "设备未授权：请在手机上点击弹窗「允许 USB 调试 / 允许调试」后重试。若未弹窗，请拔插 USB 线或在开发者选项撤销授权后重连。"
	case strings.Contains(msg, "offline"):
		return "设备离线：请检查 USB 线是否插好、手机是否已解锁，或无线调试开关是否已开启。"
	case strings.Contains(msg, "not found") || strings.Contains(msg, "no devices"):
		return "未找到设备：请确认设备已通过 USB 连接并开启「USB 调试」。"
	case strings.Contains(msg, "connect failed") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "closed"):
		return "连接不稳定：与设备的 ADB 连接中断，请稍后重试；若反复失败，请重新「连接设备」。"
	case strings.Contains(msg, "protocol fault"):
		// adb pair 失败时（IP 错误、端口错误、配对码错误、设备未响应）统一报
		// protocol fault，无法进一步区分。给出覆盖最常见原因的友好提示。
		return "配对失败：请核对设备 IP、配对端口和 6 位配对码是否与手机「无线调试」页一致；配对码为一次性且有时效，若已用过或超时请刷新后重试。"
	default:
		return msg
	}
}

// handleScrcpyPair 处理无线调试配对：接收设备 IP、配对端口与 6 位配对码。
func (r *Router) handleScrcpyPair(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.adbConn == nil {
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
	if err := r.adbConn.Pair(req.Context(), body.Host, body.PairPort, body.Code); err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
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
	if r.adbConn == nil {
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
	if err := r.adbConn.Connect(req.Context(), body.Host, body.Port); err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
		return
	}
	// 无线调试的连接端口是动态分配的，不能假设固定 5555，也绝不能执行
	// adb tcpip 5555（会把无线调试设备打成 unauthorized、破坏授权）。连接成功后
	// 解析该 IP 真实在线的端口回传给前端，前端据此拼 serial 并发起控制/持久化。
	// 解析失败时退回用户填写端口，不阻断连接成功的结果。
	realPort := body.Port
	if p, err := r.adbConn.ResolveOnlinePort(req.Context(), body.Host); err == nil {
		realPort = p
	}
	_ = r.svc.Store.Audit(u.ID, "scrcpy-connect", "", body.Host)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "port": realPort})
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
	if r.adbConn == nil {
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
	online, err := r.adbConn.IsDeviceOnline(req.Context(), serial)
	if err != nil {
		fail(w, http.StatusInternalServerError, "status failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"online": online})
}

// handleScrcpyUsbDevices 返回当前 adb 中通过 USB 有线连接的设备序列号列表、授权状态，
// 以及当前已在线的无线 ip:port 列表（供「传统模式」识别已切过 5555 的设备，避免重复切 tcpip）。
func (r *Router) handleScrcpyUsbDevices(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.adbConn == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	serials, err := r.adbConn.UsbSerials(req.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "list usb devices failed: "+err.Error())
		return
	}
	resp := map[string]any{"serials": serials}
	if r.scrcpy != nil {
		if usbDevs, err := r.scrcpy.UsbDevices(req.Context()); err == nil {
			resp["devices"] = usbDevs
		}
		if onlineDevs, err := r.scrcpy.OnlineDevices(req.Context()); err == nil {
			resp["tcpipOnline"] = onlineDevs
		}
	}
	writeJSON(w, http.StatusOK, resp)
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
	if r.adbConn == nil {
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
	if err := r.adbConn.Tcpip(req.Context(), body.Serial, body.Port); err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
		return
	}
	_ = r.svc.Store.Audit(u.ID, "scrcpy-tcpip", "", body.Serial)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "port": body.Port})
}

// handleScrcpyOnlineDevices 返回 adb 中处于 device（已配对+连接，可立即用于
// 观看/群控）状态的设备 serial 列表。serial 即无线设备的 ip:port。
//
// 供前端「一键镜像」判定哪些设备可被自动拉起屏幕控制，无需设备预先处于
// 控屏状态（后端 /api/scrcpy/ws 的 prepare 会自动部署+启动 scrcpy-server）。
func (r *Router) handleScrcpyOnlineDevices(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.adbConn == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	serials, err := r.adbConn.OnlineDevices(req.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "query adb devices: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"serials": serials})
}
