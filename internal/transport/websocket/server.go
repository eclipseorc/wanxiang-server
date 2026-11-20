// Package websocket 负责设备端和管理端的 WebSocket 接入、消息转发与会话生命周期管理。
// server.go 处理连接与路由；commands.go 集中定义可扩展的 com 命令协议。
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/gorilla/websocket"
)

const (
	// writeWait 是写出 JSON 时给单次写操作的超时时间，防止慢连接长期占用 goroutine。
	writeWait = 5 * time.Second
	// pongWait 是客户端发来 Pong 或服务端读到消息时允许的最长空闲时间。
	pongWait = 70 * time.Second
	// pingEvery 是服务端定期发送 WebSocket 控制帧 Ping 的时间间隔。
	pingEvery = 55 * time.Second
	// maxMessagesPerSecond 限制单条连接的入站消息速率，抑制单连接洪泛。
	maxMessagesPerSecond = 100
	// 设备上报的状态文本与命令回执在广播前会被截断，避免单条消息放大成全网洪泛。
	maxPayloadBytes = 1024
	// 设备与管理端连接数上限，防止 ticket 被批量兑换后耗尽进程资源。
	maxDeviceConnections = 1000
	maxAdminConnections  = 200
)

// client 表示一个已升级的单连接客户端，既可能是设备端，也可能是管理端。
type client struct {
	conn *websocket.Conn
	mu   sync.Mutex
	kind string
	id   string
	done chan struct{}
	once sync.Once

	// 以下两个字段只由读循环所在的 goroutine 访问，不需要额外加锁。
	readWindow time.Time
	readCount  int
}

// newClient 创建一个底层 WebSocket 连接对象，并记录它的角色和主体 ID。
func newClient(conn *websocket.Conn, kind, id string) *client {
	return &client{conn: conn, kind: kind, id: id, done: make(chan struct{})}
}

// send 统一发 JSON 消息，并在发送前设置写超时，避免长时间卡住。
func (c *client) send(value any) error {
	if c.conn == nil {
		return errors.New("websocket connection is not initialized")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteJSON(value)
}

// close 安全关闭连接：只允许首次调用关闭，避免重复 close 造成 panic。
func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}

// refreshReadDeadline 在成功读到任意入站消息后顺延空闲超时。
// gorilla/websocket 只允许在读循环所在的 goroutine 设置读超时，因此
// 调用点必须位于 device()/admin() 的 ReadJSON 之后。
func (c *client) refreshReadDeadline() {
	if c.conn == nil {
		return
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
}

// allowMessage 用固定一秒窗口限制单连接的入站消息数量。
// 仅发送应用层心跳（而不回应控制帧 Ping）的原生客户端也不会再被误断开。
func (c *client) allowMessage(now time.Time) bool {
	if now.Sub(c.readWindow) >= time.Second {
		c.readWindow, c.readCount = now, 0
	}
	c.readCount++
	return c.readCount <= maxMessagesPerSecond
}

// Hub 是 WebSocket 中枢，保存设备连接与管理员连接。
type Hub struct {
	store *sqlite.Store
	mu    sync.RWMutex

	// connector 处理设备端上行 adb-ready 事件：设备在本地完成「无线调试自配对 +
	// 固化 5555」后通知服务端，由 connector 执行 adb connect。该能力由装配层注入
	// （logic.AdbConnector），未注入时收到 adb-ready 只记录日志、不执行 connect。
	// 这里用最小接口，避免 websocket 包直接依赖 logic 层的具体类型。
	connector AdbReadyConnector

	// devices 表示当前在线的设备连接，以 deviceID 为键。
	devices map[string]*client
	// deviceIPs 记录设备最近一次心跳上报的本机 IP，用于「业务在线但 ADB 离线」
	// 的判断（把 deviceId 关联到 adb 的 serial 前缀 IP）。IP 可能随网络变化，
	// 因此每次心跳动态刷新，只做内存态、不落库。
	deviceIPs map[string]string
	// adbOnline 记录每个 deviceId 当前的 ADB 连接状态，由 ADBMonitor 扫描更新，
	// 供设备快照与状态广播携带「业务在线但 ADB 离线」的区分标记。
	adbOnline map[string]bool
	// admins 表示所有已认证的「管理端通道」连接（管理员 + 普通用户），
	// key 是客户端对象本身。它不等于管理员集合：普通用户连 /ws/admin 后
	// 只能看到自己获授权的设备，任何可见性判断都必须走 Store.CanAccess。
	admins map[*client]sqlite.User
}

// AdbReadyConnector 是处理设备「ADB 就绪」事件的最小接口。由装配层注入
// logic.AdbConnector 的实现，把「adb connect」的执行逻辑收敛到 logic 层，
// 避免 websocket 包直接依赖具体实现。返回 connected 表示设备是否已进入 adb 在线。
type AdbReadyConnector interface {
	ConnectAfterReady(ctx context.Context, deviceID, ip string, port int) (connected bool, err error)
}

// NewHub 创建一个新的 WebSocket 中枢，并在内存中保留设备/管理员连接映射。
func NewHub(store *sqlite.Store) *Hub {
	return &Hub{
		store:     store,
		devices:   make(map[string]*client),
		deviceIPs: make(map[string]string),
		adbOnline: make(map[string]bool),
		admins:    make(map[*client]sqlite.User),
	}
}

// WithAdbConnector 注入 adb-ready 事件的处理器（logic.AdbConnector）。
// 不注入时 adb-ready 消息会被忽略（仅记日志），不影响转发服务运行。
func (h *Hub) WithAdbConnector(c AdbReadyConnector) *Hub {
	h.connector = c
	return h
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
	allowedOrigins := os.Getenv("WS_ALLOWED_ORIGINS")
	for _, allowed := range strings.Split(allowedOrigins, ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed != "" && allowed == origin {
			return true
		}
	}
	// 只记录 Origin 与配置名，绝不打印 URL（其中含有一次性 ticket）。
	log.Printf("websocket origin rejected origin=%s configured=%q path=%s", origin, allowedOrigins, r.URL.Path)
	return false
}

// upgrade 使用一次性 ticket 完成 WebSocket 握手，并确保只有合法设备/管理员才可连接。
func (h *Handler) upgrade(w http.ResponseWriter, r *http.Request, kind string) (*client, error) {
	rawTicket := r.URL.Query().Get("ticket")
	if rawTicket == "" {
		http.Error(w, "ticket required", http.StatusUnauthorized)
		return nil, sqlite.ErrNotFound
	}

	// 先校验 Origin，避免被拒绝的握手白白消耗一次性 ticket。
	// upgrader.CheckOrigin 会再校验一次；此处仅为提前拦截并留下明确日志。
	if !originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return nil, errors.New("origin not allowed")
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
	if old == nil && len(h.hub.devices) >= maxDeviceConnections {
		h.hub.mu.Unlock()
		log.Printf("websocket device connection limit reached device_id=%s", c.id)
		_ = c.send(map[string]string{"type": "error", "message": "server busy"})
		c.close()
		return
	}
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
		if !c.allowMessage(time.Now()) {
			log.Printf("websocket device message rate exceeded device_id=%s limit=%d/s", c.id, maxMessagesPerSecond)
			return
		}
		// 收到任意业务消息都刷新空闲超时，不只依赖控制帧 Pong。
		c.refreshReadDeadline()
		h.handleDevice(c, message)
	}
}

// admin 是管理端 WebSocket 入口：登录后推送设备快照，并持续处理管理指令。
// 注意：普通用户也允许连接此通道，以便实时查看被授权的设备；因此这里
// 刻意不做 role=admin 校验，所有数据访问由 CanAccess 逐条授权。
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
	if len(h.hub.admins) >= maxAdminConnections {
		h.hub.mu.Unlock()
		log.Printf("websocket admin connection limit reached user_id=%s", user.ID)
		_ = c.send(map[string]string{"type": "error", "message": "server busy"})
		c.close()
		return
	}
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
		if !c.allowMessage(time.Now()) {
			log.Printf("websocket admin message rate exceeded user_id=%s limit=%d/s", user.ID, maxMessagesPerSecond)
			return
		}
		// 收到任意业务消息都刷新空闲超时，不只依赖控制帧 Pong。
		c.refreshReadDeadline()
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

// removeDevice 在设备断开时清理在线状态并广播离线事件。
func (h *Handler) removeDevice(c *client) {
	h.hub.mu.Lock()
	current := h.hub.devices[c.id] == c
	if current {
		delete(h.hub.devices, c.id)
		delete(h.hub.deviceIPs, c.id)
	}
	h.hub.mu.Unlock()
	if current {
		_ = h.hub.store.SetOnline(c.id, false)
		h.hub.broadcastDevice(c.id, "device-offline", nil)
		log.Printf("websocket device disconnected device_id=%s", c.id)
	}
	c.close()
}

// removeAdmin 在管理员断开时移除管理员映射。
func (h *Handler) removeAdmin(c *client) {
	h.hub.mu.Lock()
	delete(h.hub.admins, c)
	h.hub.mu.Unlock()
	log.Printf("websocket admin disconnected user_id=%s", c.id)
	c.close()
}

// handleDevice 处理设备端消息；设备消息通常是心跳、状态上报或命令回执。
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
		if rtt < 0 || rtt > 60000 {
			rtt = 0
		}
		status, _ := message["status"].(string)
		if len(status) > maxPayloadBytes {
			status = status[:maxPayloadBytes]
		}
		// 记录设备上报的本机 IP，供 ADB 掉线判断使用（deviceId → IP 内存映射）。
		if ip, _ := message["ip"].(string); ip != "" {
			if len(ip) > 64 {
				ip = ip[:64]
			}
			h.hub.mu.Lock()
			h.hub.deviceIPs[c.id] = ip
			h.hub.mu.Unlock()
		}
		// 设备上报 MDM 纳管状态（Device Owner 与否），落库供管理端分流。
		if mdm, ok := message["mdmEnabled"].(bool); ok {
			if err := h.hub.store.SetDeviceMdmEnabled(c.id, mdm); err != nil {
				log.Printf("device mdm state update failed device_id=%s: %v", c.id, err)
			}
		}
		if err := h.hub.store.UpdateDeviceState(c.id, battery, rtt, status); err != nil {
			log.Printf("device heartbeat state update failed device_id=%s: %v", c.id, err)
		}
		_ = c.send(map[string]any{"type": "heartbeat-ack", "sentAt": message["sentAt"]})
		h.hub.broadcastDevice(c.id, "device-state", map[string]any{
			"battery": battery, "rttMs": rtt, "status": status, "lastSeen": time.Now().Unix(),
		})
	case "adb-ready":
		h.handleAdbReady(c, message)
	case "mdm-pull-policy":
		// 设备端（连接建立后）主动拉取当前全局 MDM 策略。
		h.sendMdmPolicies(c)
	case "mdm-command-result":
		// 设备端执行完 MDM 安装/卸载后回传结果，转发给所有管理端。
		payload := map[string]any{"deviceId": c.id}
		for _, key := range []string{"action", "packageName", "ok", "error"} {
			if v, ok := message[key]; ok {
				payload[key] = boundedPayload(v)
			}
		}
		h.hub.broadcastDevice(c.id, "mdm-command-result", payload)
	case "status":
		h.hub.broadcastDevice(c.id, "device-state", boundedPayload(message["data"]))
	case "command-result":
		// 只挑必要字段转发，避免设备提交的任意字段被放大广播给所有管理端。
		payload := make(map[string]any, 4)
		for _, key := range []string{"commandId", "requestId", "ok", "result"} {
			if value, ok := message[key]; ok {
				payload[key] = boundedPayload(value)
			}
		}
		h.hub.broadcastDevice(c.id, "command-result", payload)
	}
}

// handleAdbReady 处理设备端上行的「ADB 就绪」事件：设备在本地完成「无线调试
// 自配对 + 固化 5555」后通知服务端，服务端据此执行 adb connect IP:5555，把设备
// 拉进 adb 连接列表（MDM 全自动路径）。connect 逻辑由注入的 connector 执行；
// 未注入或执行失败时只记日志、不阻塞连接，也不会回退到其它路径。
func (h *Handler) handleAdbReady(c *client, message map[string]any) {
	ip, _ := message["lan_ip"].(string)
	port := intValue(message["adb_port"])
	if port <= 0 {
		port = 5555
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		_ = c.send(map[string]string{"type": "adb-ready-ack", "connected": "false", "reason": "empty lan_ip"})
		return
	}

	if h.hub.connector == nil {
		log.Printf("adb-ready received but connector not configured device_id=%s ip=%s", c.id, ip)
		_ = c.send(map[string]string{"type": "adb-ready-ack", "connected": "false", "reason": "connector not configured"})
		return
	}

	connected, err := h.hub.connector.ConnectAfterReady(context.Background(), c.id, ip, port)
	if err != nil {
		log.Printf("adb-ready connect failed device_id=%s ip=%s: %v", c.id, ip, err)
		_ = c.send(map[string]string{"type": "adb-ready-ack", "connected": "false", "reason": "connect failed"})
		return
	}

	if connected {
		log.Printf("adb-ready connected device_id=%s ip=%s:%d", c.id, ip, port)
		// 更新内存态 adbOnline，供管理端快照与广播立即反映 adb 上线。
		h.hub.mu.Lock()
		h.hub.adbOnline[c.id] = true
		h.hub.mu.Unlock()
		h.hub.broadcastDevice(c.id, "device-adb-state", map[string]any{"adbOnline": true, "ip": ip})
	}
	_ = c.send(map[string]any{"type": "adb-ready-ack", "connected": strconv.FormatBool(connected)})
}

// snapshotDevice 在设备快照中附加 ADB 在线状态（内存态，由 ADBMonitor 维护），
// 让前端区分「业务在线」「ADB 掉线」等状态。内嵌 sqlite.Device 保留其全部
// JSON 字段与标签；adbOnline 字段由内嵌的 Device.AdbOnline 承载，这里仅用
// 内嵌结构体做类型别名，避免重复定义 JSON tag 冲突。
type snapshotDevice struct {
	sqlite.Device
}

// sendSnapshot 把当前管理员可见的设备列表一次性发给客户端，方便浏览器初始化状态。
func (h *Handler) sendSnapshot(c *client, user sqlite.User) {
	devices, err := h.hub.store.ListDevices()
	if err != nil {
		log.Printf("load websocket device snapshot failed user_id=%s: %v", user.ID, err)
		return
	}
	// 先在锁外做权限过滤（DB 查询较慢，不应持锁）。
	visible := make([]snapshotDevice, 0, len(devices))
	for _, device := range devices {
		allowed, err := h.hub.store.CanAccess(user.ID, device.ID, false)
		if err != nil {
			log.Printf("check device access failed user_id=%s device_id=%s: %v", user.ID, device.ID, err)
			continue
		}
		if allowed {
			// AdbOnline 默认取落库的 adb_online（持久化），避免进程重启后内存态
			// 尚未扫描到就把「未授权/未连接」设备误判为 true（可操作）。
			visible = append(visible, snapshotDevice{Device: device})
		}
	}
	// 再在锁内补充 adbOnline 状态（内存态，比落库更实时）与设备上报 IP。
	h.hub.mu.RLock()
	for i := range visible {
		if v, ok := h.hub.adbOnline[visible[i].ID]; ok {
			visible[i].AdbOnline = v
		}
		// 设备心跳上报的 IP 比落库的 adb_ip 更实时；落库为空时用上报值兜底，
		// 让前端始终能拿到一个可拼 serial 的 IP。
		if visible[i].AdbIP == "" {
			if ip, ok := h.hub.deviceIPs[visible[i].ID]; ok {
				visible[i].AdbIP = ip
			}
		}
	}
	h.hub.mu.RUnlock()
	_ = c.send(map[string]any{"type": "device-list", "devices": visible})
}

// handleAdmin 处理管理员端消息：包括权限校验、设备控制与命令下发。
func (h *Handler) handleAdmin(c *client, user sqlite.User, message map[string]any) {
	switch message["type"] {
	case commandType:
		h.handleCommand(c, message)
	case "device-control":
		h.handleDeviceControl(c, user, message)
	case "mdm-policy-push":
		h.handleMdmPolicyPush(c, user, message)
	case "mdm-device-command":
		h.handleMdmDeviceCommand(c, user, message)
	case "ping":
		_ = c.send(map[string]any{"type": "pong", "sentAt": message["sentAt"]})
	}
}

// handleMdmDeviceCommand 处理管理端对单台 MDM 设备下发的安装/卸载指令：
// 把指令转发给指定 deviceId 的设备连接，由设备端 PackageInstaller 静默执行。
// 仅管理员可用；设备必须在线（存在连接）才能送达。
func (h *Handler) handleMdmDeviceCommand(c *client, user sqlite.User, message map[string]any) {
	if user.Role != "admin" {
		_ = c.send(map[string]string{"type": "error", "message": "admin only"})
		return
	}
	deviceID, _ := message["deviceId"].(string)
	action, _ := message["action"].(string)
	if deviceID == "" || (action != "install" && action != "uninstall") {
		_ = c.send(map[string]string{"type": "error", "message": "invalid mdm device command"})
		return
	}

	h.hub.mu.RLock()
	device := h.hub.devices[deviceID]
	h.hub.mu.RUnlock()
	if device == nil {
		_ = c.send(map[string]any{
			"type": "mdm-command-result", "deviceId": deviceID,
			"ok": false, "error": "device offline",
		})
		return
	}

	// 转发给设备端，附上管理端回传所需的信息。
	payload := map[string]any{"type": "mdm-device-command", "action": action}
	for _, key := range []string{"packageName", "apkId", "token", "downloadUrl"} {
		if v, ok := message[key]; ok {
			payload[key] = v
		}
	}
	if err := device.send(payload); err != nil {
		_ = c.send(map[string]any{
			"type": "mdm-command-result", "deviceId": deviceID,
			"ok": false, "error": "send failed",
		})
		return
	}
	_ = c.send(map[string]any{
		"type": "mdm-command-result", "deviceId": deviceID,
		"ok": true, "sent": true,
	})
	_ = h.hub.store.Audit(user.ID, "mdm-device-"+action, deviceID, "")
}

// handleMdmPolicyPush 处理管理端下发的「策略已变更」通知：读库后把最新策略
// 推送给所有在线的 Device Owner 设备。仅管理员可触发；普通用户无权改策略。
func (h *Handler) handleMdmPolicyPush(c *client, user sqlite.User, message map[string]any) {
	if user.Role != "admin" {
		_ = c.send(map[string]string{"type": "error", "message": "admin only"})
		return
	}
	h.pushMdmPolicies()
	_ = c.send(map[string]any{"type": "mdm-policy-pushed"})
}

// sendMdmPolicies 把当前全局 MDM 策略打包下发给单台设备（设备端主动拉取）。
func (h *Handler) sendMdmPolicies(c *client) {
	policies, err := h.hub.store.ListMdmPolicies()
	if err != nil {
		log.Printf("load mdm policies for device failed device_id=%s: %v", c.id, err)
		_ = c.send(map[string]any{"type": "mdm-policy", "policies": map[string]string{}})
		return
	}
	out := make(map[string]string, len(policies))
	for _, p := range policies {
		out[p.Key] = p.Value
	}
	_ = c.send(map[string]any{"type": "mdm-policy", "policies": out})
}

// pushMdmPolicies 把当前全局 MDM 策略推送给所有在线的 Device Owner 设备。
// 读取一次策略、广播给所有 MDM 设备，避免逐设备查库。
func (h *Handler) pushMdmPolicies() {
	policies, err := h.hub.store.ListMdmPolicies()
	if err != nil {
		log.Printf("load mdm policies for push failed: %v", err)
		return
	}
	out := make(map[string]string, len(policies))
	for _, p := range policies {
		out[p.Key] = p.Value
	}

	h.hub.mu.RLock()
	targets := make([]*client, 0)
	for id, device := range h.hub.devices {
		mdm, err := h.hub.store.DeviceByID(id)
		if err == nil && mdm.MdmEnabled {
			targets = append(targets, device)
		}
	}
	h.hub.mu.RUnlock()

	for _, device := range targets {
		_ = device.send(map[string]any{"type": "mdm-policy", "policies": out})
	}
}

// handleCommand 负责把 message 解析为统一的 CommandMessage，再调用命令分发器处理。
func (h *Handler) handleCommand(c *client, raw map[string]any) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		_ = c.send(newCommandError("", commandInvalid, "invalid command message"))
		return
	}
	var message CommandMessage
	if err := json.Unmarshal(encoded, &message); err != nil {
		_ = c.send(newCommandError("", commandInvalid, "invalid command message"))
		return
	}
	response := dispatchCommand(message)
	if err := c.send(response); err != nil {
		log.Printf("websocket command response failed kind=%s id=%s command=%s: %v", c.kind, c.id, response.Command, err)
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

// handleDeviceControl 处理管理端下发的设备控制消息：断开或启用在线设备。
// 权限由 CanAccess(manage=true) 判定，启用/停用状态直接写入数据库并广播。
func (h *Handler) handleDeviceControl(c *client, user sqlite.User, message map[string]any) {
	deviceID, _ := message["deviceId"].(string)
	action, _ := message["action"].(string)
	if deviceID == "" || (action != "disconnect" && action != "enable") {
		_ = c.send(map[string]string{"type": "error", "message": "invalid device control"})
		return
	}

	allowed, err := h.hub.store.CanAccess(user.ID, deviceID, true)
	if err != nil || !allowed {
		_ = c.send(map[string]string{"type": "error", "message": "not authorized"})
		return
	}

	// 启用/停用状态由转发进程直接写库，与在线状态同源，不再经 HTTP 进程中转。
	if err := h.hub.store.SetEnabled(deviceID, action == "enable"); err != nil {
		log.Printf("set device enabled failed user_id=%s device_id=%s: %v", user.ID, deviceID, err)
		if errors.Is(err, sqlite.ErrNotFound) {
			_ = c.send(map[string]string{"type": "error", "message": "device not found"})
		} else {
			_ = c.send(map[string]string{"type": "error", "message": "operation failed"})
		}
		return
	}

	// 断开时若设备在线，发 disconnect 帧并关闭连接。
	if action == "disconnect" {
		h.hub.mu.RLock()
		device := h.hub.devices[deviceID]
		h.hub.mu.RUnlock()
		if device != nil {
			_ = device.send(map[string]string{"type": "disconnect", "reason": "admin"})
			device.close()
		}
	}

	_ = h.hub.store.Audit(user.ID, action, deviceID, "")
	h.hub.broadcastDevice(deviceID, "device-admin-state", map[string]string{"state": action})
}

// boundedPayload 在广播前限制设备上报内容的体积，避免单条入站消息
// 被放大成对所有管理端的大流量推送。只处理一层，刻意不递归解析深嵌套结构。
func boundedPayload(value any) any {
	if text, ok := value.(string); ok {
		if len(text) > maxPayloadBytes {
			return text[:maxPayloadBytes]
		}
		return text
	}
	if fields, ok := value.(map[string]any); ok {
		out := make(map[string]any, len(fields))
		for key, item := range fields {
			if len(out) >= 32 {
				break
			}
			if text, ok := item.(string); ok && len(text) > maxPayloadBytes {
				out[key] = text[:maxPayloadBytes]
			} else {
				out[key] = item
			}
		}
		return out
	}
	return value
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
