package adminapi

import (
	"net/http"
	"os"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// DPC 组件的固定标识与签名校验和。扫码纳管时，系统据此找到并校验要激活为
// Device Owner 的应用。
//
//   - DpcComponentName：万象客户端的 DeviceAdminReceiver 组件。
//   - DpcSignatureChecksum：APK 签名证书 SHA-256 的 URL-safe base64 校验和
//     （算法：取证书 SHA-256 指纹十六进制 → 转字节 → base64 → +/ 换成 -_ → 去 =）。
//
// 当前默认值是 debug 签名（仅开发验证）。切 release 签名后必须同步更新
// signature checksum，否则扫码纳管会因签名不匹配而失败。
const (
	defaultDpcComponentName     = "com.wanxiang.client/.MyDeviceAdminReceiver"
	defaultDpcSignatureChecksum = "zef7y_Q16geGzmKF0RzBHrjm1RGS1VeleILZQU1cve4"
)

// dpcQrcodeReq 是生成 DPC 纳管二维码的入参。
type dpcQrcodeReq struct {
	// WifiSSID 与 WifiPassword 用于开机向导阶段自动联网，便于下载/激活 DPC。
	WifiSSID         string `json:"wifiSsid"`
	WifiPassword     string `json:"wifiPassword"`
	WifiSecurityType string `json:"wifiSecurityType"`
	// TenantToken 是纳管完成后设备归属租户的初始注册凭证。
	TenantToken string `json:"tenantToken"`
	// PackageDownloadURL 是 DPC 客户端 APK 的直接下载地址（可选）。官方仅要求
	// 它是可下载到 APK 的 URL，不强制 HTTPS（内网 http 亦可）。留空则不下发该
	// 字段，系统默认 DPC 已预装、跳过下载环节。
	PackageDownloadURL string `json:"packageDownloadUrl"`
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
	body.PackageDownloadURL = strings.TrimSpace(body.PackageDownloadURL)

	// WiFi SSID 必填：否则开机向导无法联网、后续纳管流程走不通。
	if body.WifiSSID == "" {
		fail(w, http.StatusBadRequest, "wifiSsid is required")
		return
	}
	// 安全类型缺省 WPA（最常见的场景）；密码为空时视为开放网络。
	if body.WifiSecurityType == "" {
		body.WifiSecurityType = "WPA"
	}

	// DPC 组件名与签名校验和：默认取常量（debug 签名），可用环境变量覆盖，
	// 便于切换 release 签名或适配不同 DPC 组件而不改代码。
	dpcComponent := strings.TrimSpace(os.Getenv("DPC_COMPONENT_NAME"))
	if dpcComponent == "" {
		dpcComponent = defaultDpcComponentName
	}
	dpcChecksum := strings.TrimSpace(os.Getenv("DPC_SIGNATURE_CHECKSUM"))
	if dpcChecksum == "" {
		dpcChecksum = defaultDpcSignatureChecksum
	}

	payload := map[string]any{
		"android.app.extra.PROVISIONING_WIFI_SSID":          body.WifiSSID,
		"android.app.extra.PROVISIONING_WIFI_SECURITY_TYPE": body.WifiSecurityType,
		// 告诉系统要激活哪个应用作为 Device Owner，及其签名校验和。
		// 组件名与签名校验和是扫码纳管成功的关键：缺了系统找不到/不信任 DPC。
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME":     dpcComponent,
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM": dpcChecksum,
		"android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE": map[string]any{
			"tenant_token": body.TenantToken,
		},
	}
	// 只有提供了密码才写入该字段；开放网络不含密码项。
	if body.WifiPassword != "" {
		payload["android.app.extra.PROVISIONING_WIFI_PASSWORD"] = body.WifiPassword
	}
	// 下载地址由表单运行时提供，留空则不下发（系统默认 DPC 已预装、跳过下载）。
	if body.PackageDownloadURL != "" {
		payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION"] = body.PackageDownloadURL
	}

	_ = r.svc.Store.Audit(u.ID, "dpc-qrcode", "", "")
	writeJSON(w, http.StatusOK, dpcQrcodeResp{Payload: payload})
}
