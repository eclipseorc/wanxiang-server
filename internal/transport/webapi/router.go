// Package webapi 提供面向安卓设备端的 HTTP 接口。
package webapi

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
)

const maxRequestBody = 1 << 20

const (
	// ticketMaxPerWindow 限制设备换票频率，抑制对 deviceId 的批量探测与刷库。
	ticketMaxPerWindow = 60
	ticketWindow       = time.Minute
	limiterMaxEntries  = 10000
)

// Router 把设备端 HTTP 请求映射到应用层服务。
type Router struct {
	svc     *logic.Service
	tickets *fixedWindowLimiter
}

func New(s *logic.Service) *Router {
	return &Router{
		svc:     s,
		tickets: newFixedWindowLimiter(ticketMaxPerWindow, ticketWindow),
	}
}

// Handler 返回设备端路由：目前只有换票一个接口。
func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/device/health", r.health)
	mux.HandleFunc("POST /api/device/ws-ticket", r.deviceTicket)
	return mux
}

func (r *Router) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "web-api"})
}

// deviceTicket 为设备签发一次性 WebSocket 连接票据。
// 设备不登录：以 deviceId 作为唯一凭证，首次调用即自动登记设备（附带型号）。
func (r *Router) deviceTicket(w http.ResponseWriter, req *http.Request) {
	if !r.tickets.allow("device-ticket:"+clientKey(req), time.Now()) {
		fail(w, http.StatusTooManyRequests, "too many ticket requests")
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
		Model    string `json:"model"`
	}
	if decode(w, req, &body) != nil || strings.TrimSpace(body.DeviceID) == "" {
		fail(w, http.StatusBadRequest, "deviceId required")
		return
	}
	if err := r.svc.RegisterDevice(body.DeviceID, body.Model); err != nil {
		failInput(w, "register device", err)
		return
	}
	ticket, err := r.svc.CreateTicket("device", body.DeviceID)
	if err != nil {
		failInternal(w, "issue device ticket", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// failInternal 把底层错误写入服务端日志，只向客户端返回通用消息。
func failInternal(w http.ResponseWriter, action string, err error) {
	log.Printf("webapi %s failed: %v", action, err)
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

type windowCount struct {
	count int
	start time.Time
}

// fixedWindowLimiter 用于换票端点的固定窗口限速。
type fixedWindowLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]windowCount
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		limit:   limit,
		window:  window,
		entries: make(map[string]windowCount),
	}
}

func (l *fixedWindowLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.entries) >= limiterMaxEntries {
		for k, entry := range l.entries {
			if now.Sub(entry.start) >= l.window {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= limiterMaxEntries {
			return false
		}
	}
	entry := l.entries[key]
	if now.Sub(entry.start) >= l.window {
		entry = windowCount{start: now}
	}
	entry.count++
	l.entries[key] = entry
	return entry.count <= l.limit
}

// clientKey 从 RemoteAddr 提取客户端 IP。
// 刻意不信任 X-Forwarded-For：只有明确部署了可信反向代理时才应改用它。
func clientKey(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return strings.ToLower(host)
}
