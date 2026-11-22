package adminapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestDpcQrcodeIncludesAdminFields 验证 DPC 纳管二维码的 payload 必须包含
// 组件名（PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME）与签名校验和
// （PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM）。这两个字段缺失会导致
// 扫码纳管时系统找不到/不信任 DPC 应用，属于「漏配即失败」的关键项。
func TestDpcQrcodeIncludesAdminFields(t *testing.T) {
	h := newTestRouter(t)
	token := loginAdmin(t, h)

	rec := request(t, h, http.MethodPost, "/api/admin/dpc/qrcode",
		`{"wifiSsid":"office-wifi","wifiPassword":"secret","wifiSecurityType":"WPA","tenantToken":"tenant-123","packageDownloadUrl":"http://192.168.1.100:9002/wanxiang-client.apk"}`,
		token)
	if rec.Code != http.StatusOK {
		t.Fatalf("dpc/qrcode: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// 组件名与签名校验和必须存在且非空。
	comp, _ := resp.Payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME"].(string)
	if comp == "" {
		t.Fatalf("payload missing PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME: %v", resp.Payload)
	}
	checksum, _ := resp.Payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM"].(string)
	if checksum == "" {
		t.Fatalf("payload missing PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM: %v", resp.Payload)
	}

	// 表单传入下载地址时，应下发 PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION。
	downloadURL, _ := resp.Payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION"].(string)
	if downloadURL != "http://192.168.1.100:9002/wanxiang-client.apk" {
		t.Fatalf("PACKAGE_DOWNLOAD_LOCATION = %q, want the supplied URL", downloadURL)
	}

	// WiFi 密码应正确写入。
	if pwd, _ := resp.Payload["android.app.extra.PROVISIONING_WIFI_PASSWORD"].(string); pwd != "secret" {
		t.Fatalf("WIFI_PASSWORD = %q, want secret", pwd)
	}

	// tenant_token 应存在于 ADMIN_EXTRAS_BUNDLE 中。
	extras, ok := resp.Payload["android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE"].(map[string]any)
	if !ok {
		t.Fatalf("ADMIN_EXTRAS_BUNDLE missing or wrong type: %v", resp.Payload["android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE"])
	}
	if extras["tenant_token"] != "tenant-123" {
		t.Fatalf("tenant_token = %v, want tenant-123", extras["tenant_token"])
	}
}

// TestDpcQrcodeRequiresWifiSsid 验证 WiFi SSID 为必填项。
func TestDpcQrcodeRequiresWifiSsid(t *testing.T) {
	h := newTestRouter(t)
	token := loginAdmin(t, h)

	rec := request(t, h, http.MethodPost, "/api/admin/dpc/qrcode",
		`{"wifiSsid":""}`, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for missing wifiSsid", rec.Code)
	}
}

// TestDpcQrcodeOmitsDownloadLocationWhenEmpty 验证下载地址留空时不下发
// PACKAGE_DOWNLOAD_LOCATION 字段（系统据此跳过下载、默认 DPC 已预装）。
func TestDpcQrcodeOmitsDownloadLocationWhenEmpty(t *testing.T) {
	h := newTestRouter(t)
	token := loginAdmin(t, h)

	rec := request(t, h, http.MethodPost, "/api/admin/dpc/qrcode",
		`{"wifiSsid":"office-wifi","tenantToken":"tenant-123"}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("dpc/qrcode: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION"]; ok {
		t.Fatalf("PACKAGE_DOWNLOAD_LOCATION should be omitted when empty: %v", resp.Payload)
	}
}
