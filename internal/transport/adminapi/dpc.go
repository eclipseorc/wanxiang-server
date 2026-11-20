package adminapi

import (
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// dpcQrcodeReq 是生成 DPC 纳管二维码的入参。
// 注意：这里刻意不接收「组件名」与「签名校验和」——当前阶段由 App 自身
// 在 Device Owner provisioning 流程里处理（或后续再由后台补齐），二维码
// 内容先只承载联网与租户归属信息。
type dpcQrcodeReq struct {
	// WifiSSID 与 WifiPassword 用于开机向导阶段自动联网，便于下载/激活 DPC。
	WifiSSID         string `json:"wifiSsid"`
	WifiPassword     string `json:"wifiPassword"`
	WifiSecurityType string `json:"wifiSecurityType"`
	// TenantToken 是纳管完成后设备归属租户的初始注册凭证。
	TenantToken string `json:"tenantToken"`
}

// dpcQrcodeResp 返回给前端、用于渲染二维码的 provisioning JSON。
type dpcQrcodeResp struct {
	// Payload 是二维码实际承载的原始 JSON（前端用二维码库直接渲染）。
	Payload map[string]any `json:"payload"`
}

// handleDpcQrcode 处理 POST /api/admin/dpc/qrcode：拼装 Android Device Owner
// provisioning 所需的二维码 JSON，返回给前端渲染。
func (r *Router) handleDpcQrcode(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}

	var body dpcQrcodeReq
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	body.WifiSSID = strings.TrimSpace(body.WifiSSID)
	body.WifiPassword = strings.TrimSpace(body.WifiPassword)
	body.WifiSecurityType = strings.TrimSpace(body.WifiSecurityType)
	body.TenantToken = strings.TrimSpace(body.TenantToken)

	// WiFi SSID 必填：否则开机向导无法联网、后续纳管流程走不通。
	if body.WifiSSID == "" {
		fail(w, http.StatusBadRequest, "wifiSsid is required")
		return
	}
	// 安全类型缺省 WPA（最常见的场景）；密码为空时视为开放网络。
	if body.WifiSecurityType == "" {
		body.WifiSecurityType = "WPA"
	}

	payload := map[string]any{
		"android.app.extra.PROVISIONING_WIFI_SSID":          body.WifiSSID,
		"android.app.extra.PROVISIONING_WIFI_SECURITY_TYPE": body.WifiSecurityType,
		"android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE": map[string]any{
			"tenant_token": body.TenantToken,
		},
	}
	// 只有提供了密码才写入该字段；开放网络不含密码项。
	if body.WifiPassword != "" {
		payload["android.app.extra.PROVISIONING_WIFI_PASSWORD"] = body.WifiPassword
	}

	_ = r.svc.Store.Audit(u.ID, "dpc-qrcode", "", "")
	writeJSON(w, http.StatusOK, dpcQrcodeResp{Payload: payload})
}
