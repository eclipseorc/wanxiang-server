package websocket

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/gorilla/websocket"
)

const (
	// writeWait 是写出 JSON 时给单次写操作的超时时间，防止连接卡住导致 goroutine 堵死。
	writeWait = 10 * time.Second
	// pongWait 是客户端发来 Pong 或服务端读到消息时允许的最长空闲时间。
	pongWait = 70 * time.Second
	// pingEvery 是服务端定期发送 WebSocket 控制帧 Ping 的时间间隔。
	pingEvery = 55 * time.Second
)

// client 表示一个已升级的单连接客户端，既可能是设备端，也可能是管理端。
type client struct {
	conn *websocket.Conn
	mu   sync.Mutex
	kind string
	id   string
	done chan struct{}
	once sync.Once
}

// newClient 创建一个底层 WebSocket 连接对象，并记录它的角色和主体 ID。
func newClient(conn *websocket.Conn, kind, id string) *client {
	return &client{conn: conn, kind: kind, id: id, done: make(chan struct{})}
}

// send 统一发 JSON 消息，并在发送前设置写超时，避免长时间卡住。
func (c *client) send(value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteJSON(value)
}

// close 安全关闭连接：只允许首次调用关闭，避免重复 close 造成 panic。
func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// Hub 是 WebSocket 中枢，保存设备连接、管理员连接和当前监看会话状态。
type Hub struct {
	store *sqlite.Store
	mu    sync.RWMutex

	// devices 表示当前在线的设备连接，以 deviceID 为键。
	devices map[string]*client
	// admins 表示当前在线的管理员连接，key 是客户端对象本身。
	admins map[*client]sqlite.User

	// watchMu 保护 watch 相关全局状态；viewer 表示当前正在看设备的管理员客户端。
	watchMu   sync.Mutex
	viewer    *client
	sessionID string
	deviceID  string
}

// NewHub 创建一个新的 WebSocket 中枢，并在内存中保留设备/管理员连接映射。
func NewHub(store *sqlite.Store) *Hub {
	return &Hub{
		store:   store,
		devices: make(map[string]*client),
		admins:  make(map[*client]sqlite.User),
	}
}

// Handler 负责把 HTTP 请求升级成 WebSocket 连接，并把来自设备/管理员的消息交给对应处理函数。
type Handler struct{ hub *Hub }

// NewHandler 创建 WebSocket 处理器。
func NewHandler(hub *Hub) *Handler { return &Handler{hub: hub} }

// ServeMux 注册设备端和管理员端的 WebSocket 路由。
func (h *Handler) ServeMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/device", h.device)
	mux.HandleFunc("/ws/admin", h.admin)
	return mux
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     originAllowed,
}

// originAllowed 用于限制 WebSocket 来源，避免跨站请求被接受。
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	for _, allowed := range strings.Split(os.Getenv("WS_ALLOWED_ORIGINS"), ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed != "" && allowed == origin {
			return true
		}
	}
	return false
}

// upgrade 使用一次性 ticket 完成 WebSocket 握手，并确保只有合法设备/管理员才可连接。
func (h *Handler) upgrade(w http.ResponseWriter, r *http.Request, kind string) (*client, error) {
	rawTicket := r.URL.Query().Get("ticket")
	if rawTicket == "" {
		http.Error(w, "ticket required", http.StatusUnauthorized)
		return nil, sqlite.ErrNotFound
	}

	ticket, err := h.hub.store.ConsumeTicket(sqlite.Hash(rawTicket))
	if err != nil || ticket.Type != kind {
		http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
		return nil, sqlite.ErrNotFound
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	return newClient(conn, kind, ticket.SubjectID), nil
}

// device 是设备端 WebSocket 入口：连接成功后记录在线状态，并开始读取设备消息。
func (h *Handler) device(w http.ResponseWriter, r *http.Request) {
	c, err := h.upgrade(w, r, "device")
	if err != nil {
		log.Printf("websocket device handshake rejected remote=%s: %v", r.RemoteAddr, err)
		return
	}

	enabled, err := h.hub.store.DeviceEnabled(c.id)
	if err != nil || !enabled {
		_ = c.send(map[string]string{"type": "error", "message": "device disabled or unknown"})
		c.close()
		return
	}

	h.hub.mu.Lock()
	old := h.hub.devices[c.id]
	h.hub.devices[c.id] = c
	h.hub.mu.Unlock()
	if old != nil {
		_ = old.send(map[string]string{"type": "replaced"})
		old.close()
	}

	_ = h.hub.store.SetOnline(c.id, true)
	_ = c.send(map[string]any{"type": "joined", "deviceId": c.id})
	log.Printf("websocket device connected device_id=%s remote=%s", c.id, r.RemoteAddr)
	h.hub.broadcastDevice(c.id, "device-online", nil)
	go h.pingLoop(c)
	defer h.removeDevice(c)

	for {
		var message map[string]any
		if err := c.conn.ReadJSON(&message); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("websocket device read failed device_id=%s: %v", c.id, err)
			}
			return
		}
		h.handleDevice(c, message)
	}
}

// admin 是管理员 WebSocket 入口：登录后推送设备快照，并持续处理管理指令。
func (h *Handler) admin(w http.ResponseWriter, r *http.Request) {
	c, err := h.upgrade(w, r, "admin")
	if err != nil {
		log.Printf("websocket admin handshake rejected remote=%s: %v", r.RemoteAddr, err)
		return
	}

	user, err := h.hub.store.UserByID(c.id)
	if err != nil {
		c.close()
		return
	}

	h.hub.mu.Lock()
	h.hub.admins[c] = user
	h.hub.mu.Unlock()
	defer h.removeAdmin(c)

	log.Printf("websocket admin connected user_id=%s remote=%s", user.ID, r.RemoteAddr)
	h.sendSnapshot(c, user)
	go h.pingLoop(c)

	for {
		var message map[string]any
		if err := c.conn.ReadJSON(&message); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("websocket admin read failed user_id=%s: %v", user.ID, err)
			}
			return
		}
		h.handleAdmin(c, user, message)
	}
}

// pingLoop 是 WebSocket 控制帧 heartbeat 管理器；只负责连接保活，不负责业务命令。
func (h *Handler) pingLoop(c *client) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.mu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.mu.Unlock()
			if err != nil {
				c.close()
				return
			}
		}
	}
}

// removeDevice 在设备断开时清理在线状态、广播离线事件，并释放正在进行的 watch。
func (h *Handler) removeDevice(c *client) {
	h.hub.mu.Lock()
	current := h.hub.devices[c.id] == c
	if current {
		delete(h.hub.devices, c.id)
	}
	h.hub.mu.Unlock()
	if current {
		_ = h.hub.store.SetOnline(c.id, false)
		h.hub.broadcastDevice(c.id, "device-offline", nil)
		h.hub.endWatchForDevice(c.id)
		log.Printf("websocket device disconnected device_id=%s", c.id)
	}
	c.close()
}

// removeAdmin 在管理员断开时移除管理员映射，并结束这位管理员持有的观看会话。
func (h *Handler) removeAdmin(c *client) {
	h.hub.mu.Lock()
	delete(h.hub.admins, c)
	h.hub.mu.Unlock()

	h.hub.watchMu.Lock()
	sessionID, viewer := h.hub.sessionID, h.hub.viewer
	h.hub.watchMu.Unlock()
	if viewer == c && sessionID != "" {
		h.hub.endWatch(sessionID)
	}
	log.Printf("websocket admin disconnected user_id=%s", c.id)
	c.close()
}

// handleDevice 处理设备端消息；设备消息通常是心跳、状态上报、WebRTC 信令或命令回执。
func (h *Handler) handleDevice(c *client, message map[string]any) {
	switch message["type"] {
	case commandType:
		h.handleCommand(c, message)
	case "join":
		_ = c.send(map[string]string{"type": "joined", "deviceId": c.id})
	case "out":
		_ = c.send(map[string]string{"type": "out-ack"})
		c.close()
	case "heartbeat":
		battery := intValue(message["battery"])
		if battery < 0 || battery > 100 {
			battery = 0
		}
		rtt := int64(intValue(message["rttMs"]))
		status, _ := message["status"].(string)
		if err := h.hub.store.UpdateDeviceState(c.id, battery, rtt, status); err != nil {
			log.Printf("device heartbeat state update failed device_id=%s: %v", c.id, err)
		}
		_ = c.send(map[string]any{"type": "heartbeat-ack", "sentAt": message["sentAt"]})
		h.hub.broadcastDevice(c.id, "device-state", map[string]any{
			"battery": battery, "rttMs": rtt, "status": status, "lastSeen": time.Now().Unix(),
		})
	case "status":
		h.hub.broadcastDevice(c.id, "device-state", message["data"])
	case "command-result":
		h.hub.broadcastDevice(c.id, "command-result", message)
	case "watch-ready", "watch-denied", "watch-stopped", "offer", "answer", "candidate":
		h.hub.watchMu.Lock(); viewer, sessionID, deviceID := h.hub.viewer, h.hub.sessionID, h.hub.deviceID; h.hub.watchMu.Unlock()
		if viewer == nil || deviceID != c.id || sessionID == "" || message["sessionId"] != sessionID {
			return
		}
		_ = viewer.send(message)
		if message["type"] == "watch-denied" || message["type"] == "watch-stopped" {
			h.hub.endWatch(sessionID)
		}
	}
}

// sendSnapshot 把当前管理员可见的设备列表一次性发给客户端，方便浏览器初始化状态。
func (h *Handler) sendSnapshot(c *client, user sqlite.User) {
	devices, err := h.hub.store.ListDevices()
	if err != nil {
		log.Printf("load websocket device snapshot failed user_id=%s: %v", user.ID, err)
		return
	}
	visible := make([]sqlite.Device, 0, len(devices))
	for _, device := range devices {
		allowed, err := h.hub.store.CanAccess(user.ID, device.ID, false)
		if err != nil {
			log.Printf("check device access failed user_id=%s device_id=%s: %v", user.ID, device.ID, err)
			continue
		}
		if allowed {
			visible = append(visible, device)
		}
	}
	_ = c.send(map[string]any{"type": "device-list", "devices": visible})
}

// handleAdmin 处理管理员端消息：包括权限校验、启动 watch、转发 WebRTC 信令、发送 com 命令。
func (h *Handler) handleAdmin(c *client, user sqlite.User, message map[string]any) {
	switch message["type"] {
	case commandType:
		h.handleCommand(c, message)
	case "ping":
		_ = c.send(map[string]any{"type": "pong", "sentAt": message["sentAt"]})
	case "watch":
		deviceID, _ := message["deviceId"].(string)
		allowed, err := h.hub.store.CanAccess(user.ID, deviceID, false)
		if err != nil || !allowed {
			_ = c.send(map[string]string{"type": "error", "message": "not authorized"})
			return
		}

		h.hub.mu.RLock()
		device := h.hub.devices[deviceID]
		h.hub.mu.RUnlock()
		if device == nil {
			_ = c.send(map[string]string{"type": "error", "message": "device offline"})
			return
		}

		sessionID := newSessionID()
		if err := h.hub.store.StartWatch(sessionID, deviceID, user.ID); err != nil {
			_ = c.send(map[string]string{"type": "error", "message": err.Error()})
			return
		}
		h.hub.watchMu.Lock()
		if h.hub.viewer != nil {
			h.hub.watchMu.Unlock()
			_ = h.hub.store.EndWatch(sessionID)
			_ = c.send(map[string]string{"type": "error", "message": "watch already active"})
			return
		}
		h.hub.viewer, h.hub.sessionID, h.hub.deviceID = c, sessionID, deviceID
		h.hub.watchMu.Unlock()

		_ = h.hub.store.Audit(user.ID, "watch-request", deviceID, sessionID)
		_ = device.send(map[string]any{"type": "watch-request", "sessionId": sessionID, "requestedBy": user.ID})
		_ = c.send(map[string]string{"type": "watch-started", "sessionId": sessionID, "deviceId": deviceID})
	case "watch-stop":
		h.hub.watchMu.Lock()
		sessionID, viewer := h.hub.sessionID, h.hub.viewer
		h.hub.watchMu.Unlock()
		if viewer == c {
			h.hub.endWatch(sessionID)
		}
	case "offer", "answer", "candidate":
		h.hub.watchMu.Lock()
		deviceID, sessionID, viewer := h.hub.deviceID, h.hub.sessionID, h.hub.viewer
		h.hub.watchMu.Unlock()
		if viewer != c || sessionID == "" || message["sessionId"] != sessionID {
			return
		}
		h.hub.mu.RLock()
		device := h.hub.devices[deviceID]
		h.hub.mu.RUnlock()
		if device != nil {
			_ = device.send(message)
		}
	}
}

// handleCommand 负责把 message 解析为统一的 CommandMessage，再调用命令分发器处理。
func (h *Handler) handleCommand(c *client, raw map[string]any) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		_ = c.send(commandError("", commandInvalid, "invalid command message"))
		return
	}
	var message CommandMessage
	if err := json.Unmarshal(encoded, &message); err != nil {
		_ = c.send(commandError("", commandInvalid, "invalid command message"))
		return
	}
	response := dispatchCommand(message)
	if err := c.send(response); err != nil {
		log.Printf("websocket command response failed kind=%s id=%s command=%s: %v", c.kind, c.id, response.Command, err)
	}
}

// endWatch 结束当前全局 watch 会话，通知双方连接，释放全局状态，并写入审计日志。
func (h *Hub) endWatch(sessionID string) {
	if sessionID == "" {
		return
	}
	h.watchMu.Lock()
	if h.sessionID != sessionID {
		h.watchMu.Unlock()
		return
	}
	viewer, deviceID := h.viewer, h.deviceID
	h.viewer, h.deviceID, h.sessionID = nil, "", ""
	h.watchMu.Unlock()

	_ = h.store.EndWatch(sessionID)
	if viewer != nil {
		_ = viewer.send(map[string]string{"type": "watch-ended", "sessionId": sessionID})
	}
	h.mu.RLock()
	device := h.devices[deviceID]
	h.mu.RUnlock()
	if device != nil {
		_ = device.send(map[string]string{"type": "watch-stop", "sessionId": sessionID})
	}
	_ = h.store.Audit("system", "watch-ended", deviceID, sessionID)
}

// endWatchForDevice 用于设备断开或被禁用时，自动结束这个设备对应的 watch 会话。
func (h *Hub) endWatchForDevice(id string) {
	h.watchMu.Lock()
	sessionID, deviceID := h.sessionID, h.deviceID
	h.watchMu.Unlock()
	if id == deviceID {
		h.endWatch(sessionID)
	}
}

// broadcastDevice 把设备状态广播给所有有权限查看该设备的管理员。
func (h *Hub) broadcastDevice(id, messageType string, data any) {
	event := map[string]any{"type": messageType, "deviceId": id, "data": data}
	h.hubAdminSnapshot(func(c *client, user sqlite.User) {
		allowed, err := h.store.CanAccess(user.ID, id, false)
		if err == nil && allowed {
			_ = c.send(event)
		}
	})
}

// hubAdminSnapshot 遍历当前所有管理员连接，并对每个管理员执行访问检查回调。
func (h *Hub) hubAdminSnapshot(fn func(*client, sqlite.User)) {
	h.mu.RLock()
	admins := make([]struct {
		client *client
		user   sqlite.User
	}, 0, len(h.admins))
	for c, user := range h.admins {
		admins = append(admins, struct {
			client *client
			user   sqlite.User
		}{client: c, user: user})
	}
	h.mu.RUnlock()
	for _, admin := range admins {
		fn(admin.client, admin.user)
	}
}

// RunControlPoller 按固定频率轮询 SQLite 中待处理的控制事件，并同步到在线设备状态。
func (h *Hub) RunControlPoller(stop <-chan struct{}) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			events, err := h.store.PendingEvents()
			if err != nil {
				log.Printf("control event poll failed: %v", err)
				continue
			}
			for _, event := range events {
				h.applyEvent(event)
				if err := h.store.MarkEvent(event.ID); err != nil {
					log.Printf("mark control event processed failed event_id=%d: %v", event.ID, err)
				}
			}
		}
	}
}

// applyEvent 根据控制事件类型执行操作：例如断开设备、广播设备状态或清理 watch。
func (h *Hub) applyEvent(event sqlite.ControlEvent) {
	h.mu.RLock()
	device := h.devices[event.DeviceID]
	h.mu.RUnlock()
	if event.Kind == "disconnect" {
		if device != nil {
			_ = device.send(map[string]string{"type": "disconnect", "reason": "admin"})
			device.close()
		}
		h.endWatchForDevice(event.DeviceID)
	}
	h.broadcastDevice(event.DeviceID, "device-admin-state", map[string]string{"state": event.Kind})
}

// intValue 把接口层读取的任意值统一转换成 int，兼容 float64 / int / int64 等类型。
func intValue(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}

// newSessionID 生成一次性的 watch 会话 ID，用于单次监看请求的全局唯一标识。
func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(buf)
}
