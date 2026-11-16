// Package adminapi 提供管理 HTTP API、Bearer 认证与基础安全响应头。
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

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

const maxRequestBody = 1 << 20

// Router 把 HTTP 请求映射到应用层服务。
type Router struct {
	svc     *logic.Service
	limiter *loginLimiter
	tickets *fixedWindowLimiter
}

func New(s *logic.Service) *Router {
	return &Router{
		svc:     s,
		limiter: newLoginLimiter(),
		tickets: newFixedWindowLimiter(ticketMaxPerWindow, ticketWindow),
	}
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
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
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
	case "users":
		r.createUser(w, req, u)
	case "groups":
		r.groups(w, req, u)
	case "groups/users":
		r.addUserGroup(w, req, u)
	case "groups/devices":
		r.addDeviceGroup(w, req, u)
	case "group-access":
		r.setGroupAccess(w, req, u)
	default:
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
			out = append(out, device)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (r *Router) createUser(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
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
