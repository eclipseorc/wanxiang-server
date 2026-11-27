// Package websocket 负责设备端和管理端的 WebSocket 接入、消息转发与会话生命周期管理。
// server.go 处理连接与路由；commands.go 集中定义可扩展的 com 命令协议。
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// uiTreeWaiters 挂起等待设备回传极速无障碍控件树（dump-ui-tree -> ui-tree-result）的请求。
	uiTreeWaiters map[string]chan []byte
}

// AdbReadyConnector 是处理设备「ADB 就绪」事件的最小接口。由装配层注入
// logic.AdbConnector 的实现，把「adb connect」的执行逻辑收敛到 logic 层，
// 避免 websocket 包直接依赖具体实现。返回 connected 表示设备是否已进入 adb 在线。
type AdbReadyConnector interface {
	ConnectAfterReady(ctx context.Context, deviceID, ip string, port int) (connected bool, err error)
}

// AdbPairConnector 是可选扩展接口：供设备无障碍读到 6 位配对码后，由服务端直接执行 adb pair + connect。
type AdbPairConnector interface {
	PairAndConnect(ctx context.Context, deviceID, ip string, pairPort int, code string, connectPort int) (finalPort int, connected bool, err error)
}

// NewHub 创建一个新的 WebSocket 中枢，并在内存中保留设备/管理员连接映射。
func NewHub(store *sqlite.Store) *Hub {
	return &Hub{
		store:         store,
		devices:       make(map[string]*client),
		deviceIPs:     make(map[string]string),
		adbOnline:     make(map[string]bool),
		admins:        make(map[*client]sqlite.User),
		uiTreeWaiters: make(map[string]chan []byte),
	}
}

// RequestUITree 通过 WebSocket 向在线设备请求极速抓取当前屏幕的无障碍控件树（约 50ms）。
// 若设备未在线、未开启无障碍或 2.5 秒内未返回，则返回错误，由调用方自动回退到 ADB dump。
func (h *Hub) RequestUITree(ctx context.Context, deviceID string) ([]byte, error) {
	if deviceID == "" {
		return nil, errors.New("empty deviceId")
	}
	reqID := fmt.Sprintf("ut-%d", time.Now().UnixNano())
	ch := make(chan []byte, 1)

	h.mu.Lock()
	dev := h.devices[deviceID]
	if dev == nil {
		h.mu.Unlock()
		return nil, errors.New("device websocket offline")
	}
	h.uiTreeWaiters[reqID] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.uiTreeWaiters, reqID)
		h.mu.Unlock()
	}()

	if err := dev.send(map[string]any{"type": "dump-ui-tree", "reqId": reqID}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(2500 * time.Millisecond)
	defer timer.Stop()

	select {
	case data := <-ch:
		if len(data) == 0 {
			return nil, errors.New("accessibility ui tree unavailable")
		}
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("ui tree request timeout")
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
	conn.SetReadLimit(512 << 10)
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
		// 设备断开时同步将 adb_online 置为 false（保留原 IP 与端口供下次重连）。
		if dev, err := h.hub.store.DeviceByID(c.id); err == nil {
			_ = h.hub.store.SetDeviceAdb(c.id, dev.AdbIP, dev.AdbPort, false)
		}
		h.hub.mu.Lock()
		h.hub.adbOnline[c.id] = false
		h.hub.mu.Unlock()
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

// handleDevice 按 commands.go 中的 deviceRoutes 注册表分发设备端消息。
func (h *Handler) handleDevice(c *client, message map[string]any) {
	msgType, _ := message["type"].(string)
	if fn, ok := deviceRoutes[msgType]; ok {
		fn(h, c, message)
	}
}

// syncDeviceIPChange 在心跳发现设备局域网 IP 变化时（例如断网重连 Wi-Fi 后 DHCP 分配了新 IP），
// 立即把新 IP 写入数据库并广播给管理端，避免后台仍拿着旧 IP 显示在线或点击控屏失败；
// 同时异步尝试用新 IP 自动重连 ADB。
func (h *Handler) syncDeviceIPChange(deviceID, newIP string) {
	dev, err := h.hub.store.DeviceByID(deviceID)
	if err != nil || dev.AdbIP == newIP {
		return
	}
	oldIP := dev.AdbIP
	port := dev.AdbPort
	if port <= 0 {
		port = 5555
	}
	mode := dev.AdbMode

	if mode != "" {
		_ = h.hub.store.SetDeviceAdbWithMode(deviceID, newIP, port, false, mode)
	} else {
		_ = h.hub.store.SetDeviceAdb(deviceID, newIP, port, false)
	}
	h.hub.mu.Lock()
	h.hub.adbOnline[deviceID] = false
	h.hub.mu.Unlock()
	h.hub.broadcastDevice(deviceID, "device-adb-state", map[string]any{
		"adbOnline": false,
		"ip":        newIP,
		"port":      port,
		"adbMode":   mode,
	})
	log.Printf("device ip changed device_id=%s old_ip=%s new_ip=%s port=%d", deviceID, oldIP, newIP, port)

	if h.hub.connector == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		connected, err := h.hub.connector.ConnectAfterReady(ctx, deviceID, newIP, port)
		if mode != "" {
			_ = h.hub.store.SetDeviceAdbWithMode(deviceID, newIP, port, connected && err == nil, mode)
		}
		if err != nil || !connected {
			log.Printf("device ip change adb reconnect pending device_id=%s ip=%s:%d err=%v", deviceID, newIP, port, err)
			return
		}
		h.hub.mu.Lock()
		h.hub.adbOnline[deviceID] = true
		h.hub.mu.Unlock()
		h.hub.broadcastDevice(deviceID, "device-adb-state", map[string]any{
			"adbOnline": true,
			"ip":        newIP,
			"port":      port,
			"adbMode":   mode,
		})
		log.Printf("device ip change adb reconnected device_id=%s ip=%s:%d", deviceID, newIP, port)
	}()
}

// handleAdbReady 处理设备端上行的「ADB 就绪」事件：设备在本地完成「无线调试
// 自配对 + 固化 5555」后通知服务端，服务端据此执行 adb connect IP:5555，把设备
// 拉进 adb 连接列表（自动配对路径）。connect 逻辑由注入的 connector 执行；
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

	h.hub.mu.Lock()
	h.hub.deviceIPs[c.id] = ip
	h.hub.mu.Unlock()

	if h.hub.connector == nil {
		log.Printf("adb-ready received but connector not configured device_id=%s ip=%s", c.id, ip)
		_ = c.send(map[string]string{"type": "adb-ready-ack", "connected": "false", "reason": "connector not configured"})
		return
	}

	adbMode, _ := message["adb_mode"].(string)
	adbMode = strings.TrimSpace(adbMode)
	if adbMode != "wireless" && adbMode != "tcpip" {
		adbMode = "wireless"
	}

	connected, err := h.hub.connector.ConnectAfterReady(context.Background(), c.id, ip, port)
	if err != nil {
		log.Printf("adb-ready connect failed device_id=%s ip=%s: %v", c.id, ip, err)
		_ = h.hub.store.SetDeviceAdbWithMode(c.id, ip, port, false, adbMode)
		h.hub.broadcastDevice(c.id, "device-adb-state", map[string]any{"adbOnline": false, "ip": ip, "port": port, "adbMode": adbMode})
		_ = c.send(map[string]string{"type": "adb-ready-ack", "connected": "false", "reason": "connect failed"})
		return
	}

	_ = h.hub.store.SetDeviceAdbWithMode(c.id, ip, port, connected, adbMode)
	if connected {
		log.Printf("adb-ready connected device_id=%s ip=%s:%d mode=%s", c.id, ip, port, adbMode)
		// 更新内存态 adbOnline，供管理端快照与广播立即反映 adb 上线。
		h.hub.mu.Lock()
		h.hub.adbOnline[c.id] = true
		h.hub.mu.Unlock()
		h.hub.broadcastDevice(c.id, "device-adb-state", map[string]any{"adbOnline": true, "ip": ip, "port": port, "adbMode": adbMode})
	} else {
		h.hub.broadcastDevice(c.id, "device-adb-state", map[string]any{"adbOnline": false, "ip": ip, "port": port, "adbMode": adbMode})
	}
	_ = c.send(map[string]any{"type": "adb-ready-ack", "connected": strconv.FormatBool(connected)})
}

// handleAdbPairRequest 处理设备无障碍读到无线调试配对码后发来的服务端配对请求：
// 异步在电脑端执行 adb pair + adb connect + pm grant WRITE_SECURE_SETTINGS，并回执 adb-pair-ack。
func (h *Handler) handleAdbPairRequest(c *client, message map[string]any) {
	ip, _ := message["lan_ip"].(string)
	ip = strings.TrimSpace(ip)
	code, _ := message["code"].(string)
	code = strings.TrimSpace(code)
	pairPort := intValue(message["pair_port"])
	connectPort := intValue(message["connect_port"])
	if ip == "" || pairPort <= 0 || code == "" {
		_ = c.send(map[string]any{"type": "adb-pair-ack", "ok": false, "error": "缺少 lan_ip / pair_port / code"})
		return
	}
	pairer, ok := h.hub.connector.(AdbPairConnector)
	if !ok || pairer == nil {
		_ = c.send(map[string]any{"type": "adb-pair-ack", "ok": false, "error": "服务端未配置 ADB 配对器"})
		return
	}

	h.hub.mu.Lock()
	h.hub.deviceIPs[c.id] = ip
	h.hub.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		finalPort, connected, err := pairer.PairAndConnect(ctx, c.id, ip, pairPort, code, connectPort)
		if err != nil || !connected {
			errMsg := "配对连接失败"
			if err != nil {
				errMsg = err.Error()
			}
			// 若 adb pair 本身已成功、仅因旧 connectPort 失效未连上，标记 paired=true，
			// 让手机端用本机 NsdManager 探测最新实时端口再上报 adb-ready。
			paired := strings.Contains(errMsg, "配对成功")
			log.Printf("adb-pair-request failed device_id=%s ip=%s:%d paired=%v err=%s", c.id, ip, pairPort, paired, errMsg)
			_ = c.send(map[string]any{"type": "adb-pair-ack", "ok": false, "paired": paired, "error": errMsg})
			return
		}
		h.hub.mu.Lock()
		h.hub.adbOnline[c.id] = true
		h.hub.mu.Unlock()
		h.hub.broadcastDevice(c.id, "device-adb-state", map[string]any{
			"adbOnline": true,
			"ip":        ip,
			"port":      finalPort,
			"adbMode":   "wireless",
		})
		log.Printf("adb-pair-request ok device_id=%s ip=%s:%d", c.id, ip, finalPort)
		_ = c.send(map[string]any{"type": "adb-pair-ack", "ok": true, "port": finalPort})
	}()
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
	// 再在锁内以当前真实 WebSocket 连接（hub.devices）为准刷新在线态与 ADB 状态。
	h.hub.mu.RLock()
	for i := range visible {
		_, wsConnected := h.hub.devices[visible[i].ID]
		visible[i].Online = wsConnected
		if !wsConnected {
			visible[i].AdbOnline = false
		} else if v, ok := h.hub.adbOnline[visible[i].ID]; ok {
			visible[i].AdbOnline = v
		}
		// 设备在线时，心跳上报的实时 IP 优先于落库的旧 adb_ip（防止切网换 IP 后显示旧地址）。
		if visible[i].Online {
			if ip, ok := h.hub.deviceIPs[visible[i].ID]; ok && ip != "" {
				visible[i].AdbIP = ip
			}
		} else if visible[i].AdbIP == "" {
			if ip, ok := h.hub.deviceIPs[visible[i].ID]; ok {
				visible[i].AdbIP = ip
			}
		}
		if visible[i].AdbPort <= 0 {
			visible[i].AdbPort = 5555
		}
		// 若设备在线且已通过 5555 自动连接（或上报了 IP 且未曾被标记为 manual），默认为 auto 自动模式。
		if visible[i].AdbMode == "" && (visible[i].AdbOnline || (visible[i].Online && visible[i].AdbIP != "")) {
			visible[i].AdbMode = "auto"
		}
	}
	h.hub.mu.RUnlock()
	_ = c.send(map[string]any{"type": "device-list", "devices": visible})
}

// handleAdmin 按 commands.go 中的 adminRoutes 注册表分发管理端消息。
func (h *Handler) handleAdmin(c *client, user sqlite.User, message map[string]any) {
	msgType, _ := message["type"].(string)
	if fn, ok := adminRoutes[msgType]; ok {
		fn(h, c, user, message)
	}
}

// handleCommand 负责把 message 解析为统一的 CommandMessage，再调用命令分发器处理。
// sendDeviceScript 把设备型号对应的「自动配对」机型脚本下发给单台设备（设备端
// 主动拉取）。用设备已上报的 model（落库 devices.model）在机型脚本表的
// match_patterns 里做匹配，命中即下发完整脚本；未命中下发空脚本 + found=false，
// 设备端据此记日志、本次不自动配对。脚本字段（id/name/matchPatterns/steps/version）
// 原样透传给设备端，由设备端解析成 DeviceScript 执行。
func (h *Handler) sendDeviceScript(c *client) {
	device, err := h.hub.store.DeviceByID(c.id)
	if err != nil {
		log.Printf("load device for script failed device_id=%s: %v", c.id, err)
		_ = c.send(map[string]any{"type": "device-script", "found": false})
		return
	}
	script, err := h.hub.store.DeviceModelScriptByMatch(device.Model)
	if err != nil {
		// 未匹配到脚本（ErrNotFound）或查询失败：下发 found=false，设备端不自动配对。
		log.Printf("device script not found device_id=%s model=%q: %v", c.id, device.Model, err)
		_ = c.send(map[string]any{"type": "device-script", "found": false, "model": device.Model})
		return
	}
	_ = c.send(map[string]any{
		"type":  "device-script",
		"found": true,
		"model": device.Model,
		"script": map[string]any{
			"scriptId":      script.ID,
			"name":          script.Name,
			"matchPatterns": script.MatchPatterns,
			"steps":         json.RawMessage(script.StepsJSON),
			"version":       script.Version,
		},
	})
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
