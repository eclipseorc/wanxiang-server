// device_connect.go 提供设备级「连接/断开」端点：把原本藏在远程控制窗口里的
// 配对+连接两步抽象成独立的设备操作，成功后把 IP、端口与 adb 在线状态落库，
// 供设备清单据此显示状态并驱动「远程控制」「读取文件」按钮的可用性。
//
// 与 scrcpy.go 里的无状态 pair/connect 不同：这里把结果持久化到 devices 表
// （adb_ip / adb_port / adb_online），是「设备级连接状态」的唯一权威写入点。
package adminapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// handleDeviceConnect 完成设备「配对（可选）+ 连接」，成功后落库。
//
// 请求体（JSON）：
//   - deviceId：设备唯一标识（必填）
//   - host：设备局域网 IP（必填）
//   - pairPort：配对端口，仅首次配对需要（可选）
//   - code：6 位配对码，仅首次配对需要（可选）
//   - port：连接端口，缺省 5555（可选）
//
// 语义：若同时提供 pairPort+code 则先配对（首次）；否则视为已配对、直接连接。
// 连接成功后用 adb devices 解析该 IP 真实在线的端口并落库（无线调试端口是
// 动态分配的，不能假设固定 5555），host 落库为 adb_ip、真实端口落库为 adb_port。
func (r *Router) handleDeviceConnect(w http.ResponseWriter, req *http.Request, u sqlite.User) {
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
		DeviceID string `json:"deviceId"`
		Host     string `json:"host"`
		PairPort int    `json:"pairPort"`
		Code     string `json:"code"`
		Port     int    `json:"port"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.DeviceID = strings.TrimSpace(body.DeviceID)
	body.Host = strings.TrimSpace(body.Host)
	body.Code = strings.TrimSpace(body.Code)
	if body.DeviceID == "" || body.Host == "" {
		fail(w, http.StatusBadRequest, "deviceId and host are required")
		return
	}
	if body.Port <= 0 {
		body.Port = 5555
	}

	ctx := req.Context()
	if err := r.scrcpy.EnsureServer(ctx); err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
		return
	}

	// 首次配对：提供了配对端口与配对码才走 pair。已配对设备直接连接。
	if body.PairPort > 0 && body.Code != "" {
		if err := r.scrcpy.Pair(ctx, body.Host, body.PairPort, body.Code); err != nil {
			fail(w, http.StatusBadRequest, friendlyAdbError(err))
			return
		}
	}

	// 连接（内部已轮询等待 device 状态）。
	if err := r.scrcpy.Connect(ctx, body.Host, body.Port); err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
		return
	}

	// 连接成功后解析该 IP 真实在线的端口（无线调试端口动态分配，不能假设 5555）。
	// 解析失败（设备刚从 offline/unauthorized 转回、端口尚未就绪等）时，退回
	// 用户填写的端口作为兜底，避免因一次解析失败就判连接失败。
	realPort, err := r.scrcpy.ResolveOnlinePort(ctx, body.Host)
	if err != nil {
		realPort = body.Port
		_ = r.svc.Store.Audit(u.ID, "device-connect-resolve-port-failed", body.DeviceID, body.Host+": "+err.Error())
	}

	// 落库：记录 IP、真实端口与在线状态。
	if err := r.svc.Store.SetDeviceAdb(body.DeviceID, body.Host, realPort, true); err != nil {
		fail(w, http.StatusInternalServerError, "persist adb state failed: "+err.Error())
		return
	}

	_ = r.svc.Store.Audit(u.ID, "device-connect", body.DeviceID, body.Host)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"ip":   body.Host,
		"port": realPort,
	})
}

// handleDeviceDisconnect 断开设备 adb 连接并置为离线（保留 IP 供自动重连）。
func (r *Router) handleDeviceDisconnect(w http.ResponseWriter, req *http.Request, u sqlite.User) {
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
		DeviceID string `json:"deviceId"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.DeviceID = strings.TrimSpace(body.DeviceID)
	if body.DeviceID == "" {
		fail(w, http.StatusBadRequest, "deviceId is required")
		return
	}

	// 尝试断开（拿当前落库 IP+端口拼真实 serial）。失败不阻断状态更新。
	if device, err := r.svc.Store.DeviceByID(body.DeviceID); err == nil && device.AdbIP != "" {
		port := device.AdbPort
		if port <= 0 {
			port = 5555 // 旧数据无端口，兜底 5555
		}
		serial := fmt.Sprintf("%s:%d", device.AdbIP, port)
		if derr := r.scrcpy.Disconnect(req.Context(), serial); derr != nil {
			_ = r.svc.Store.Audit(u.ID, "device-disconnect-failed", body.DeviceID, serial+": "+derr.Error())
		}
	}

	// 置离线，保留 IP 与端口。
	if err := r.svc.Store.SetDeviceAdb(body.DeviceID, "", 0, false); err != nil {
		fail(w, http.StatusInternalServerError, "persist adb state failed: "+err.Error())
		return
	}

	_ = r.svc.Store.Audit(u.ID, "device-disconnect", body.DeviceID, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
