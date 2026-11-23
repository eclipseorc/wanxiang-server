// Package adminapi 提供管理后台 HTTP API、Bearer 认证与基础安全响应头。
// 认证中间件负责校验 session token，具体接口处理器负责输入校验和业务调用。
// 设备端接口在独立的 webapi 包中，二者不共享认证与路由。
package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/automation"
	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

const maxRequestBody = 1 << 20

// Router 把 HTTP 请求映射到应用层服务。
type Router struct {
	svc     *logic.Service
	limiter *loginLimiter
	tickets *fixedWindowLimiter
	// scrcpy 仅用于文件/APK 等「会话操作」（ListFiles/Pull/Push/Install/Shell 等），
	// 这些是控屏/文件传输会话，与「连接管理」无关，继续由 scrcpy.ADB 直接提供。
	scrcpy *scrcpy.ADB
	// adbConn 是「连接管理」（配对/连接/断开/tcpip/在线状态查询等）的统一入口，
	// 收敛到 logic.AdbConnector，使 admin-server 与 websocket-server 共享同一份实现。
	adbConn *logic.AdbConnector
	apks    *apkStore
	// automation 是自动化执行引擎(动作原语 + 流程执行),由 WithScrcpy 注入。
	automation *automation.Executor
}

func New(s *logic.Service) *Router {
	return &Router{
		svc:     s,
		limiter: newLoginLimiter(),
		tickets: newFixedWindowLimiter(ticketMaxPerWindow, ticketWindow),
		apks:    newAPKStore(),
	}
}

// WithScrcpy 注入 adb 封装，启用远程控制的配对/连接接口与会话操作。
// 同一 adb 实例同时构造连接管理入口（AdbConnector）与会话操作句柄（scrcpy），
// 并据此构造自动化执行引擎（复用 adb 能力，不额外建连接）。
func (r *Router) WithScrcpy(adb *scrcpy.ADB) *Router {
	r.scrcpy = adb
	r.adbConn = logic.NewAdbConnector(r.svc, adb)
	r.automation = automation.NewExecutor(
		r.svc.Store,
		automation.NewEngine(adb, "./data/automation-shots", buildOCR()),
		adb,
		automationMaxConcurrency(),
	)
	return r
}

// AutomationExecutor 返回自动化执行引擎(未启用 scrcpy 时为 nil),供上层启动
// 定时调度器。
func (r *Router) AutomationExecutor() *automation.Executor {
	return r.automation
}

func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", r.health)
	mux.HandleFunc("POST /api/admin/login", r.login)
	mux.Handle("/api/admin/", r.auth(http.HandlerFunc(r.admin)))
	return SecurityHeaders(mux)
}

// SecurityHeaders 为管理 API 与静态页面统一添加基础安全响应头。
// 页面使用内联脚本与内联样式，因此 script-src/style-src 保留 'unsafe-inline'；
// 迁移到外部文件并加 nonce 后可以收紧。
func SecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; " +
		"media-src 'self' blob:; " +
		"connect-src 'self' ws: wss:; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	// 只有启用内置 TLS 时下发 HSTS；由反向代理终止 TLS 时由代理负责该头。
	hsts := os.Getenv("TLS_CERT") != "" && os.Getenv("TLS_KEY") != ""
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		// 管理后台不含可缓存的静态大资源；登录响应、ticket、设备令牌与
		// 前端脚本都统一禁止缓存，避免敏感数据或旧版插件被留存。
		h.Set("Cache-Control", "no-store")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, req)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decode 解析请求体；未知字段一律拒绝，避免拼错的权限字段被静默忽略。
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func bearer(req *http.Request) string {
	h := req.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	// 下载等无法携带 Authorization 头的场景（如 <a href> 原生下载）允许
	// 通过 query 参数 token 传递，避免用 fetch 缓冲大文件导致连接被中断。
	if t := req.URL.Query().Get("token"); t != "" {
		return strings.TrimSpace(t)
	}
	return ""
}

func (r *Router) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u, err := r.svc.Authenticate(bearer(req))
		if err != nil {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, contextWithUser(req, u))
	})
}

// requireMethod 与 requireAdmin 集中处理方法和角色校验，避免各分支漏检。
func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	fail(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
}

func requireAdmin(w http.ResponseWriter, u sqlite.User) bool {
	if u.Role != "admin" {
		fail(w, http.StatusForbidden, "admin only")
		return false
	}
	return true
}

// failInternal 把底层错误写入服务端日志，只向客户端返回通用消息。
func failInternal(w http.ResponseWriter, action string, err error) {
	log.Printf("adminapi %s failed: %v", action, err)
	fail(w, http.StatusInternalServerError, "operation failed")
}

// failInput 只允许明确的输入校验错误回传给客户端。
func failInput(w http.ResponseWriter, action string, err error) {
	var inputErr *logic.InputError
	if errors.As(err, &inputErr) {
		fail(w, http.StatusBadRequest, inputErr.Message)
		return
	}
	failInternal(w, action, err)
}

func (r *Router) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "http-api"})
}

func (r *Router) login(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	key := clientKey(req) + "|" + strings.ToLower(strings.TrimSpace(body.Username))
	now := time.Now()
	if wait := r.limiter.blocked(key, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		fail(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}

	token, u, err := r.svc.Login(body.Username, body.Password)
	if err != nil {
		// 计数表满说明正在被大规模尝试，必须拒绝而不能静默放行。
		if !r.limiter.recordFailure(key, now) {
			fail(w, http.StatusTooManyRequests, "too many login attempts")
			return
		}
		// 用户名不存在与密码错误返回完全相同的响应，避免账号枚举。
		fail(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	r.limiter.recordSuccess(key)
	// 登录失败刻意不写数据库审计，避免暴力破解被放大成 DB 写入洪泛。
	_ = r.svc.Store.Audit(u.ID, "login", "", "")
	writeJSON(w, http.StatusOK, map[string]string{
		"token":  token,
		"userId": u.ID,
		"role":   u.Role,
	})
}

func (r *Router) admin(w http.ResponseWriter, req *http.Request) {
	u := userFromContext(req)
	path := strings.TrimPrefix(req.URL.Path, "/api/admin/")
	switch path {
	case "logout":
		r.logout(w, req, u)
	case "ws-ticket":
		r.adminTicket(w, req, u)
	case "devices":
		r.listDevices(w, req, u)
	case "devices/prune-offline":
		r.pruneOfflineDevices(w, req, u)
	case "users":
		r.users(w, req, u)
	case "groups":
		r.groups(w, req, u)
	case "groups/users":
		r.addUserGroup(w, req, u)
	case "groups/devices":
		r.addDeviceGroup(w, req, u)
	case "group-access":
		r.groupAccess(w, req, u)
	case "scrcpy/pair":
		r.handleScrcpyPair(w, req, u)
	case "scrcpy/connect":
		r.handleScrcpyConnect(w, req, u)
	case "scrcpy/status":
		r.handleScrcpyStatus(w, req, u)
	case "scrcpy/tcpip":
		r.handleScrcpyTcpip(w, req, u)
	case "scrcpy/usb-devices":
		r.handleScrcpyUsbDevices(w, req, u)
	case "scrcpy/online-devices":
		r.handleScrcpyOnlineDevices(w, req, u)
	case "device/connect":
		r.handleDeviceConnect(w, req, u)
	case "device/disconnect":
		r.handleDeviceDisconnect(w, req, u)
	case "files/list":
		r.handleFilesList(w, req, u)
	case "files/download":
		r.handleFilesDownload(w, req, u)
	case "files/thumb":
		r.handleFilesThumb(w, req, u)
	case "apk/upload":
		r.handleAPKUpload(w, req, u)
	case "apk/list":
		r.handleAPKList(w, req, u)
	case "apk/install":
		r.handleAPKInstall(w, req, u)
	case "apk/uninstall":
		r.handleAPKUninstall(w, req, u)
	case "sync-groups":
		r.handleSyncGroups(w, req, u)
	case "automation/flows":
		r.handleAutomationFlows(w, req, u)
	case "automation/jobs":
		r.handleAutomationJobs(w, req, u)
	case "automation/ui-tree":
		r.handleAutomationUITree(w, req, u)
	case "device-scripts":
		r.handleDeviceScripts(w, req, u)
	case "device-models":
		r.handleDeviceModelOptions(w, req, u)
	default:
		// 单资源删除路径：/api/admin/sync-groups/{id}、/api/admin/users/{id}、/api/admin/groups/{id}。
		switch {
		case strings.HasPrefix(path, "devices/"):
			r.deleteDevice(w, req, u, strings.TrimPrefix(path, "devices/"))
			return
		case strings.HasPrefix(path, "sync-groups/"):
			r.handleSyncGroup(w, req, u)
			return
		case strings.HasPrefix(path, "users/"):
			r.deleteUser(w, req, u, strings.TrimPrefix(path, "users/"))
			return
		case strings.HasPrefix(path, "groups/"):
			r.deleteGroup(w, req, u, strings.TrimPrefix(path, "groups/"))
			return
		case strings.HasPrefix(path, "automation/flows/"):
			rest := strings.TrimPrefix(path, "automation/flows/")
			// /automation/flows/{id}/run 触发执行,否则按 {id} 做单资源操作。
			if strings.HasSuffix(rest, "/run") {
				r.handleAutomationRun(w, req, u, strings.TrimSuffix(rest, "/run"))
				return
			}
			r.handleAutomationFlow(w, req, u, rest)
			return
		case strings.HasPrefix(path, "automation/jobs/"):
			r.handleAutomationJob(w, req, u, strings.TrimPrefix(path, "automation/jobs/"))
			return
		case strings.HasPrefix(path, "device-scripts/"):
			r.handleDeviceScriptDelete(w, req, u, strings.TrimPrefix(path, "device-scripts/"))
			return
		}
		http.NotFound(w, req)
	}
}

func (r *Router) logout(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if err := r.svc.Store.DeleteSession(sqlite.Hash(bearer(req))); err != nil {
		failInternal(w, "logout", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "logout", "", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) adminTicket(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	// ticket 端点不需要密码校验，必须限速，否则可被用来低成本刷库。
	if !r.tickets.allow("admin-ticket:"+clientKey(req), time.Now()) {
		fail(w, http.StatusTooManyRequests, "too many ticket requests")
		return
	}
	ticket, err := r.svc.CreateTicket("admin", u.ID)
	if err != nil {
		failInternal(w, "issue admin ticket", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
}

func (r *Router) listDevices(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	devices, err := r.svc.Store.ListDevices()
	if err != nil {
		failInternal(w, "list devices", err)
		return
	}
	out := make([]sqlite.Device, 0, len(devices))
	for _, device := range devices {
		allowed, err := r.svc.Store.CanAccess(u.ID, device.ID, false)
		if err != nil {
			log.Printf("adminapi check device access failed user_id=%s device_id=%s: %v", u.ID, device.ID, err)
			continue
		}
		if allowed {
			if !device.Online {
				device.AdbOnline = false
			}
			if device.AdbPort <= 0 {
				device.AdbPort = 5555
			}
			if device.AdbMode == "" && (device.AdbOnline || (device.Online && device.AdbIP != "")) {
				device.AdbMode = "auto"
			}
			out = append(out, device)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteDevice 删除单台设备记录。路径 DELETE /api/admin/devices/{id}。
func (r *Router) deleteDevice(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireMethod(w, req, http.MethodDelete) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		fail(w, http.StatusBadRequest, "device id required")
		return
	}
	if err := r.svc.Store.DeleteDevice(id); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			fail(w, http.StatusNotFound, "device not found")
			return
		}
		failInternal(w, "delete device", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "delete-device", id, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// pruneOfflineDevices 批量清理所有处于离线状态（online=0）的历史设备记录。路径 POST /api/admin/devices/prune-offline。
func (r *Router) pruneOfflineDevices(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	deleted, err := r.svc.Store.DeleteOfflineDevices()
	if err != nil {
		failInternal(w, "prune offline devices", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "prune-offline-devices", "", fmt.Sprintf("deleted=%d", deleted))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

// users 处理 GET /api/admin/users（账号列表）与 POST（创建用户）。
func (r *Router) users(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireAdmin(w, u) {
		return
	}
	switch req.Method {
	case http.MethodGet:
		users, err := r.svc.ListUsers()
		if err != nil {
			failInternal(w, "list users", err)
			return
		}
		// 不回传密码哈希，仅返回展示所需字段。
		out := make([]map[string]string, 0, len(users))
		for _, usr := range users {
			out = append(out, map[string]string{
				"id":        usr.ID,
				"username":  usr.Username,
				"role":      usr.Role,
				"createdAt": usr.CreatedAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		r.createUser(w, req, u)
	default:
		requireMethod(w, req, http.MethodGet, http.MethodPost)
	}
}

func (r *Router) createUser(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	id, err := r.svc.CreateUser(body.Username, body.Password)
	if err != nil {
		failInput(w, "create user", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "create-user", "", id)
	writeJSON(w, http.StatusCreated, map[string]string{"userId": id})
}

// deleteUser 删除普通用户（管理员不可删）。路径 /api/admin/users/{id}。
func (r *Router) deleteUser(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireMethod(w, req, http.MethodDelete) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if err := r.svc.DeleteUser(id); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			fail(w, http.StatusNotFound, "user not found")
			return
		}
		failInput(w, "delete user", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "delete-user", "", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deleteGroup 删除分组。路径 /api/admin/groups/{id}。
func (r *Router) deleteGroup(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireMethod(w, req, http.MethodDelete) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if err := r.svc.DeleteGroup(id); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			fail(w, http.StatusNotFound, "group not found")
			return
		}
		failInput(w, "delete group", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "delete-group", "", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) groups(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet, http.MethodPost) {
		return
	}
	if req.Method == http.MethodGet {
		groups, err := r.svc.Store.ListGroups()
		if err != nil {
			failInternal(w, "list groups", err)
			return
		}
		writeJSON(w, http.StatusOK, groups)
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if decode(w, req, &body) != nil || strings.TrimSpace(body.Name) == "" {
		fail(w, http.StatusBadRequest, "name required")
		return
	}
	id, err := randomID()
	if err != nil {
		failInternal(w, "generate group id", err)
		return
	}
	if err := r.svc.Store.CreateGroup(id, strings.TrimSpace(body.Name)); err != nil {
		log.Printf("adminapi create group failed: %v", err)
		fail(w, http.StatusConflict, "group already exists or could not be created")
		return
	}
	_ = r.svc.Store.Audit(u.ID, "create-group", "", id)
	writeJSON(w, http.StatusCreated, map[string]string{"groupId": id})
}

func (r *Router) addUserGroup(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	var body struct {
		UserID  string `json:"userId"`
		GroupID string `json:"groupId"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(body.UserID) == "" || strings.TrimSpace(body.GroupID) == "" {
		fail(w, http.StatusBadRequest, "userId and groupId required")
		return
	}
	if err := r.svc.Store.AddUserGroup(body.UserID, body.GroupID); err != nil {
		log.Printf("adminapi add user group failed: %v", err)
		fail(w, http.StatusBadRequest, "invalid user or group")
		return
	}
	_ = r.svc.Store.Audit(u.ID, "group-user-add", "", fmt.Sprintf("%s:%s", body.UserID, body.GroupID))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) addDeviceGroup(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
		GroupID  string `json:"groupId"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(body.DeviceID) == "" || strings.TrimSpace(body.GroupID) == "" {
		fail(w, http.StatusBadRequest, "deviceId and groupId required")
		return
	}
	if err := r.svc.Store.AddDeviceGroup(body.DeviceID, body.GroupID); err != nil {
		log.Printf("adminapi add device group failed: %v", err)
		fail(w, http.StatusBadRequest, "invalid device or group")
		return
	}
	_ = r.svc.Store.Audit(u.ID, "group-device-add", body.DeviceID, body.GroupID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// groupAccess 处理权限关系：GET 列表、POST 设置、DELETE 删除。
func (r *Router) groupAccess(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	switch req.Method {
	case http.MethodGet:
		if !requireAdmin(w, u) {
			return
		}
		list, err := r.svc.ListGroupAccess()
		if err != nil {
			failInternal(w, "list group access", err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodDelete:
		if !requireAdmin(w, u) {
			return
		}
		r.deleteGroupAccess(w, req, u)
	case http.MethodPost:
		r.setGroupAccess(w, req, u)
	default:
		requireMethod(w, req, http.MethodGet, http.MethodPost, http.MethodDelete)
	}
}

// deleteGroupAccess 删除一条授权关系，参数经 query 传递（userGroupId/deviceGroupId）。
func (r *Router) deleteGroupAccess(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	ug := req.URL.Query().Get("userGroupId")
	dg := req.URL.Query().Get("deviceGroupId")
	if err := r.svc.DeleteGroupAccess(ug, dg); err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			fail(w, http.StatusNotFound, "group access not found")
			return
		}
		failInput(w, "delete group access", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "group-access-delete", "", ug+"->"+dg)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) setGroupAccess(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	var body struct {
		UserGroupID   string `json:"userGroupId"`
		DeviceGroupID string `json:"deviceGroupId"`
		CanView       bool   `json:"canView"`
		CanManage     bool   `json:"canManage"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(body.UserGroupID) == "" || strings.TrimSpace(body.DeviceGroupID) == "" {
		fail(w, http.StatusBadRequest, "userGroupId and deviceGroupId required")
		return
	}
	if err := r.svc.Store.SetGroupAccess(body.UserGroupID, body.DeviceGroupID, body.CanView, body.CanManage); err != nil {
		log.Printf("adminapi set group access failed: %v", err)
		fail(w, http.StatusBadRequest, "invalid group access")
		return
	}
	_ = r.svc.Store.Audit(u.ID, "group-access-set", "",
		fmt.Sprintf("%s->%s view=%t manage=%t", body.UserGroupID, body.DeviceGroupID, body.CanView, body.CanManage))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
