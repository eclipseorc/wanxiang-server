// mdm.go 提供「应用管理」下的 MDM 策略接口：全局策略 CRUD、应用名单
// （黑/白名单）增删查、以及面向 Device Owner 设备的远程安装/卸载指令。
//
// 设计约定：
//   - 策略是「全局统一」的：一张 mdm_policies 表存七类策略，下发到所有已纳管
//     的 Device Owner 设备。设备端在连接建立时拉取一次、策略变更时由管理端经
//     WebSocket 触发推送（见 websocket 包）。
//   - 「两套逻辑并存」：远程安装/卸载按设备是否 Device Owner 分流——
//     mdm_enabled=true 的设备走 WebSocket 下发 + 设备端 PackageInstaller 静默
//     安装/卸载；否则走 adb 传统路径（复用 apk.go 的 Install/Uninstall）。
//   - 本文件只负责「策略数据的读写」与「指令的构造」；真正把消息推给设备的
//     动作由 websocket 转发进程完成（管理端经 /ws/admin 发 mdm-policy-push）。
package adminapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// mdmApkToken 是「设备端下载 APK」的一次性凭证，绑定 apkId，短时间内有效。
type mdmApkToken struct {
	apkID     string
	expiresAt time.Time
}

// mdmApkTokenStore 进程内维护 APK 下载 token（与 apkStore 同生命周期）。
type mdmApkTokenStore struct {
	mu     sync.Mutex
	tokens map[string]mdmApkToken
}

func newMdmApkTokenStore() *mdmApkTokenStore {
	return &mdmApkTokenStore{tokens: make(map[string]mdmApkToken)}
}

func (s *mdmApkTokenStore) issue(apkID string, ttl time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	token := hex.EncodeToString(raw)
	s.tokens[token] = mdmApkToken{apkID: apkID, expiresAt: time.Now().Add(ttl)}
	return token
}

func (s *mdmApkTokenStore) consume(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[token]
	if !ok {
		return "", false
	}
	delete(s.tokens, token)
	if time.Now().After(t.expiresAt) {
		return "", false
	}
	return t.apkID, true
}

// mdmPolicyKey 是七类 MDM 策略的固定 key，前后端与设备端共用同一套命名，
// 避免字符串散落各处造成不一致。
const (
	// 应用安装限制：value 为 {"mode":"blacklist"|"whitelist","packages":[...]}。
	mdmKeyInstallPolicy = "install_policy"
	// 防卸载应用：value 为 {"packages":[...]}（这些包禁止被卸载）。
	mdmKeyUninstallBlocked = "uninstall_blocked"
	// 应用运行黑名单：value 为 {"packages":[...]}（这些包被挂起/禁用）。
	mdmKeyRunBlacklist = "run_blacklist"
	// 常驻应用：value 为 {"packages":[...]}（保活、不允许卸载）。
	mdmKeyKeepAlive = "keep_alive"
	// 开机自启应用：value 为 {"packages":[...]}（开机后自动拉起）。
	mdmKeyAutoStart = "auto_start"
	// 设备权限设置：value 为整组设备权限开关状态的 JSON，结构见 mdm.go 底部注释。
	// 前端「设备权限设置」页读写、设备端 MdmPolicyManager 落地（UserRestriction 等）。
	mdmKeyDevicePerms = "device_perms"
)

// mdmPolicyList 列出所有由本模块管理的策略 key，供前端一次性拉取与设备端校验。
var mdmPolicyList = []string{
	mdmKeyInstallPolicy,
	mdmKeyUninstallBlocked,
	mdmKeyRunBlacklist,
	mdmKeyKeepAlive,
	mdmKeyAutoStart,
	mdmKeyDevicePerms,
}

// handleMdmPolicies 处理 GET /api/admin/mdm/policies（返回全部策略）与
// POST /api/admin/mdm/policies（写入/更新一条策略）。
func (r *Router) handleMdmPolicies(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireAdmin(w, u) {
		return
	}
	switch req.Method {
	case http.MethodGet:
		policies, err := r.svc.Store.ListMdmPolicies()
		if err != nil {
			failInternal(w, "list mdm policies", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
	case http.MethodPost:
		r.setMdmPolicy(w, req, u)
	default:
		requireMethod(w, req, http.MethodGet, http.MethodPost)
	}
}

// setMdmPolicy 写入一条策略：{ key, value }，value 是 JSON 字符串（包名列表或模式）。
func (r *Router) setMdmPolicy(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Key = strings.TrimSpace(body.Key)
	if body.Key == "" || !validMdmPolicyKey(body.Key) {
		fail(w, http.StatusBadRequest, "unknown policy key")
		return
	}
	if !jsonValid(body.Value) {
		fail(w, http.StatusBadRequest, "value must be valid JSON")
		return
	}
	if err := r.svc.Store.SetMdmPolicy(body.Key, body.Value); err != nil {
		failInternal(w, "set mdm policy", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "mdm-policy-set", "", body.Key)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMdmPolicyDelete 处理 DELETE /api/admin/mdm/policies/{key}。
func (r *Router) handleMdmPolicyDelete(w http.ResponseWriter, req *http.Request, u sqlite.User, key string) {
	if !requireMethod(w, req, http.MethodDelete) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if err := r.svc.Store.DeleteMdmPolicy(key); err != nil {
		if err == sqlite.ErrNotFound {
			fail(w, http.StatusNotFound, "policy not found")
			return
		}
		failInternal(w, "delete mdm policy", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "mdm-policy-delete", "", key)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// validMdmPolicyKey 校验 key 是否在已知策略集合内，拒绝未知 key 防注入。
func validMdmPolicyKey(key string) bool {
	for _, k := range mdmPolicyList {
		if k == key {
			return true
		}
	}
	return false
}

// jsonValid 用 encoding/json 做一次轻量校验，确保写入的是合法 JSON。
// 包名列表前端以 JSON 数组/对象提交，落库后设备端直接反序列化。
func jsonValid(s string) bool {
	if s == "" {
		return true
	}
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

// mdmDownloadTTL 是设备端下载 APK 凭证的有效期。设备端收到指令后应在此时长内
// 完成下载，过期则需管理端重新触发。
const mdmDownloadTTL = 10 * time.Minute

// handleMdmApkDownload 处理设备端无 session 的 APK 下载请求。
// GET /api/admin/mdm/apk-download?apkId=xxx&token=yyy
// token 一次性、绑定 apkId、短时有效；下载后即失效。
func (r *Router) handleMdmApkDownload(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	apkID := strings.TrimSpace(req.URL.Query().Get("apkId"))
	token := strings.TrimSpace(req.URL.Query().Get("token"))
	if apkID == "" || token == "" {
		fail(w, http.StatusBadRequest, "apkId and token required")
		return
	}
	gotApkID, ok := r.mdmTokens.consume(token)
	if !ok || gotApkID != apkID {
		fail(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	rec, ok := r.apks.get(apkID)
	if !ok {
		fail(w, http.StatusNotFound, "apk not found")
		return
	}
	data, err := os.ReadFile(rec.Path)
	if err != nil {
		failInternal(w, "read apk for download", err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, rec.FileName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	_, _ = w.Write(data)
}

// mdmInstallReq 是 MDM 远程安装的入参：apkId + 目标设备列表。
// mode 可强制指定 "adb" 或 "mdm"，缺省 "auto"（按设备纳管状态分流）。
type mdmInstallReq struct {
	APKID     string   `json:"apkId"`
	DeviceIDs []string `json:"deviceIds"`
	Mode      string   `json:"mode"`
}

// handleMdmInstall 处理 POST /api/admin/mdm/install：
// 对每台设备按纳管状态分流——非 MDM 走 adb install（同步返回结果），
// MDM 走 WebSocket 下发指令（返回下载 URL + token，由前端经 WS 转发）。
func (r *Router) handleMdmInstall(w http.ResponseWriter, req *http.Request, u sqlite.User) {
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
	var body mdmInstallReq
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.APKID = strings.TrimSpace(body.APKID)
	if body.APKID == "" || len(body.DeviceIDs) == 0 {
		fail(w, http.StatusBadRequest, "apkId and deviceIds are required")
		return
	}
	rec, ok := r.apks.get(body.APKID)
	if !ok {
		fail(w, http.StatusNotFound, "APK 不存在或已失效（服务重启后需重新上传）")
		return
	}

	// 逐台分流：非 MDM 设备走 adb，MDM 设备收集下发指令。
	var adbDeviceIDs []string
	var mdmDevices []map[string]any
	for _, id := range body.DeviceIDs {
		dev, err := r.svc.Store.DeviceByID(strings.TrimSpace(id))
		if err != nil {
			continue
		}
		if dev.MdmEnabled && body.Mode != "adb" {
			// MDM 路径：签发下载 token，指令由前端经 WS 转发。
			token := r.mdmTokens.issue(rec.ID, mdmDownloadTTL)
			mdmDevices = append(mdmDevices, map[string]any{
				"deviceId":    dev.ID,
				"packageName": rec.PackageName,
				"apkId":       rec.ID,
				"token":       token,
				"downloadUrl": r.mdmDownloadURL(req, rec.ID, token),
			})
		} else {
			adbDeviceIDs = append(adbDeviceIDs, dev.ID)
		}
	}

	// 非 MDM 设备走 adb 同步安装。
	var adbResults []deviceResult
	if len(adbDeviceIDs) > 0 {
		adbResults = r.runOnDevices(req, adbDeviceIDs, func(serial string) error {
			return r.scrcpy.Install(req.Context(), serial, rec.Path)
		})
	}

	_ = r.svc.Store.Audit(u.ID, "mdm-install", rec.PackageName, fmt.Sprintf("adb=%d mdm=%d", len(adbDeviceIDs), len(mdmDevices)))
	writeJSON(w, http.StatusOK, map[string]any{
		"package":    rec.PackageName,
		"adbResults": adbResults,
		"mdmDevices": mdmDevices,
	})
}

// mdmUninstallReq 是 MDM 远程卸载的入参。
type mdmUninstallReq struct {
	PackageName string   `json:"packageName"`
	DeviceIDs   []string `json:"deviceIds"`
	Mode        string   `json:"mode"`
}

// handleMdmUninstall 处理 POST /api/admin/mdm/uninstall：按纳管状态分流。
func (r *Router) handleMdmUninstall(w http.ResponseWriter, req *http.Request, u sqlite.User) {
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
	var body mdmUninstallReq
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.PackageName = strings.TrimSpace(body.PackageName)
	if body.PackageName == "" || len(body.DeviceIDs) == 0 {
		fail(w, http.StatusBadRequest, "packageName and deviceIds are required")
		return
	}

	var adbDeviceIDs []string
	var mdmDevices []map[string]any
	for _, id := range body.DeviceIDs {
		dev, err := r.svc.Store.DeviceByID(strings.TrimSpace(id))
		if err != nil {
			continue
		}
		if dev.MdmEnabled && body.Mode != "adb" {
			mdmDevices = append(mdmDevices, map[string]any{
				"deviceId":    dev.ID,
				"packageName": body.PackageName,
			})
		} else {
			adbDeviceIDs = append(adbDeviceIDs, dev.ID)
		}
	}

	var adbResults []deviceResult
	if len(adbDeviceIDs) > 0 {
		adbResults = r.runOnDevices(req, adbDeviceIDs, func(serial string) error {
			return r.scrcpy.Uninstall(req.Context(), serial, body.PackageName)
		})
	}

	_ = r.svc.Store.Audit(u.ID, "mdm-uninstall", body.PackageName, fmt.Sprintf("adb=%d mdm=%d", len(adbDeviceIDs), len(mdmDevices)))
	writeJSON(w, http.StatusOK, map[string]any{
		"package":    body.PackageName,
		"adbResults": adbResults,
		"mdmDevices": mdmDevices,
	})
}

// mdmDownloadURL 构造设备端下载 APK 的完整 URL。设备端 App 已知 admin 服务的
// host:port（与 web/ws 一并配置），这里返回 path + query，前端/设备端拼 host。
// 为简单起见返回相对路径，设备端用自己配置的 admin host 拼接。
func (r *Router) mdmDownloadURL(_ *http.Request, apkID, token string) string {
	return "/api/admin/mdm/apk-download?apkId=" + apkID + "&token=" + token
}

// device_perms 策略的 value 结构约定（前后端、Android 端 MdmPolicyManager 共用）：
//
//	{
//	  "restrictions": {          // 布尔开关，映射 DevicePolicyManager.addUserRestriction
//	    "vpn": false,            // 禁止开启VPN        → DISALLOW_CONFIG_VPN
//	    "usbDebug": false,       // 禁止USB调试/开发者模式 → DISALLOW_USB_FILE_TRANSFER + 关闭ADB
//	    "screenCapture": false,  // 禁止截屏          → DISALLOW_SCREEN_CAPTURE
//	    "factoryReset": false,   // 禁止恢复出厂设置    → DISALLOW_FACTORY_RESET
//	    "airplaneMode": false,   // 禁止开启飞行模式    → DISALLOW_AIRPLANE_MODE
//	    "wifi": false,           // 禁止启用wifi      → DISALLOW_CONFIG_WIFI
//	    "hotspot": false,        // 禁止启用热点       → DISALLOW_CONFIG_TETHERING
//	    "bluetooth": false,      // 禁止启用蓝牙       → DISALLOW_CONFIG_BLUETOOTH
//	    "usbFileTransfer": false // 禁止使用USB传输文件  → DISALLOW_USB_FILE_TRANSFER
//	  },
//	  "cameraDisabled": false,   // 禁止启用摄像头      → setCameraDisabled
//	  "mobileNetwork": "none",   // 移动网络限制："none"|"disableOn"|"disableOff"
//	  "screenAlwaysOn": false,   // 屏幕常亮          → setStayOnPluggedIn
//	  "orgName": "",             // 锁屏显示单位名称    → setOrganizationName
//	  "wifiWhitelist": []        // WIFI白名单（字符串列表）
//	}
//
// 注：手机号脱敏、三方通话、通话自动录音三项无可靠 DevicePolicyManager 标准 API，
// 前端仅作占位展示（标注「暂不支持」），不参与策略下发。

