// server.go 是 scrcpy 服务的 WS 入口，负责在浏览器与设备之间桥接
// H.264 视频流与控制事件。
//
// 流程（一次连接 = 一次完整的「准备 + 观看」）：
//
//	浏览器 WS 连接 /api/scrcpy/ws?serial=<ip:port>
//	→ 服务端同步完成 deploy（push scrcpy-server）→ start（app_process）→ forward
//	→ 建立视频 + 控制两条 TCP 通道 → 下发 codec 头 → 持续转发 H.264 帧
//	→ 接收浏览器控制事件并编码写入控制通道
//
// 配对与连接（adb pair/connect）由 admin-server 负责，本服务只处理观看会话。
package scrcpy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type activeConn struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

// Server 桥接浏览器与设备的 scrcpy 会话。
type Server struct {
	adb         *ADB
	cfg         ScrcpyConfig
	mu          sync.Mutex
	live        map[string]bool        // serial -> 是否正在被观看，防止同设备重复控制
	activeConns map[string]*activeConn // serial -> 当前活跃的浏览器 WS 连接与上下文取消函数（新连接到达时可立即中止旧连接接管）

	// 群控相关状态（见 sync.go）：由同一把 mu 保护。
	sessions map[string]*deviceSession // serial -> 活跃会话（control 通道 + 分辨率）
	groups   map[string]*syncGroup     // groupID -> 群控组
	inGroup  map[string]string         // serial -> groupID，一台设备只属于一个组
}

// NewServer 构造 scrcpy 服务。
func NewServer(adb *ADB, cfg ScrcpyConfig) *Server {
	return &Server{
		adb:         adb,
		cfg:         cfg,
		live:        make(map[string]bool),
		activeConns: make(map[string]*activeConn),
		sessions:    make(map[string]*deviceSession),
		groups:      make(map[string]*syncGroup),
		inGroup:     make(map[string]string),
	}
}

// ServeMux 返回服务路由。WS 端点不在此做 HTTP 鉴权，
// 由部署层（反向代理或前端）保证只有已登录管理员能访问。
func (s *Server) ServeMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/scrcpy/ws", s.handleWS)
	mux.HandleFunc("GET /api/scrcpy/sync", s.handleSyncWS)
	mux.HandleFunc("GET /api/scrcpy/devices", s.handleDevices)
	mux.HandleFunc("GET /api/scrcpy/debug", s.handleDebug)
	return mux
}

// handleDebug 返回当前观看/群控的内部状态（live、sessions、groups、inGroup），
// 用于诊断「群控组黑屏 / 设备被占用 / 僵尸组残留」等问题。仅调试用。
func (s *Server) handleDebug(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	live := make([]string, 0, len(s.live))
	for serial := range s.live {
		live = append(live, serial)
	}

	type sessionInfo struct {
		Serial string `json:"serial"`
		Width  uint32 `json:"width"`
		Height uint32 `json:"height"`
	}
	sessions := make([]sessionInfo, 0, len(s.sessions))
	for serial, ds := range s.sessions {
		sessions = append(sessions, sessionInfo{Serial: serial, Width: ds.width, Height: ds.height})
	}

	type groupInfo struct {
		ID      string   `json:"id"`
		Master  string   `json:"master"`
		Members []string `json:"members"`
	}
	groups := make([]groupInfo, 0, len(s.groups))
	for gid, g := range s.groups {
		members := make([]string, 0, len(g.members))
		for serial := range g.members {
			members = append(members, serial)
		}
		groups = append(groups, groupInfo{ID: gid, Master: g.master, Members: members})
	}

	inGroup := make(map[string]string, len(s.inGroup))
	for serial, gid := range s.inGroup {
		inGroup[serial] = gid
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"live":     live,
		"sessions": sessions,
		"groups":   groups,
		"inGroup":  inGroup,
	})
}

// tryAcquire 标记 serial 进入观看状态；已在观看时返回 false。
func (s *Server) tryAcquire(serial string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live[serial] {
		return false
	}
	s.live[serial] = true
	return true
}

// release 清除 serial 的观看标记。
func (s *Server) release(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, serial)
	delete(s.activeConns, serial)
}

// acquireOrPreempt 尝试获取 serial 的观看锁；若已被旧 WS 会话占用，主动取消旧上下文并关闭旧 WS，
// 等待最多 3 秒让旧会话清理退出后接管。
func (s *Server) acquireOrPreempt(serial string, conn *websocket.Conn, cancel context.CancelFunc) bool {
	ac := &activeConn{conn: conn, cancel: cancel}
	s.mu.Lock()
	if !s.live[serial] {
		s.live[serial] = true
		s.activeConns[serial] = ac
		s.mu.Unlock()
		return true
	}
	old := s.activeConns[serial]
	s.mu.Unlock()

	if old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		if old.conn != nil {
			_ = old.conn.Close()
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		s.mu.Lock()
		if !s.live[serial] {
			s.live[serial] = true
			s.activeConns[serial] = ac
			s.mu.Unlock()
			return true
		}
		s.mu.Unlock()
	}
	return false
}

func (s *Server) releaseConn(serial string, conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.activeConns[serial]
	if cur == nil || cur.conn == conn {
		delete(s.live, serial)
		delete(s.activeConns, serial)
	}
}

// handleWS 处理一次完整的远程观看会话。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	serial := strings.TrimSpace(r.URL.Query().Get("serial"))
	if serial == "" {
		writeError(w, http.StatusBadRequest, "serial is required")
		return
	}

	// 会话级编码参数覆盖：群控等多路场景可传较低帧率/码率以降低资源占用。
	// 未传的字段沿用默认配置（单台控制的高清流畅参数）。
	cfg := s.cfg
	if v := r.URL.Query().Get("maxSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxSize = n
		}
	}
	if v := r.URL.Query().Get("bitrate"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.BitRate = n
		}
	}
	if v := r.URL.Query().Get("fps"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxFPS = n
		}
	}

	// 先升级 WS，再做准备流程，这样 prepare/acquire 的错误能通过 WS 的
	// error 消息下发给前端（而非退化为一个无法带错误信息的 HTTP 响应），
	// 前端据此能展示真实原因，而不是笼统的「会话已结束」。
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 同一设备同一时刻只允许一个观看会话；若已有旧连接（如刷新页面或重连时旧连接尚未完全退出），
	// 主动取消旧连接上下文并关闭旧连接，等待其释放锁后由新连接平滑接管，避免报 "device already being controlled"。
	if !s.acquireOrPreempt(serial, conn, cancel) {
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": "device already being controlled"})
		return
	}
	defer s.releaseConn(serial, conn)

	// 完成准备流程（部署 + 启动 + forward）。
	port, err := s.prepare(ctx, serial, cfg)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	defer s.cleanup(serial, port)

	// 建立视频 + 控制两条 TCP 通道。
	tun, err := establish(ctx, port)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	defer tun.close()

	log.Printf("scrcpy viewing serial=%s port=%d", serial, port)

	// 视频：TCP -> WebSocket（读 codec 头 + 帧，转发给浏览器）。
	// resolution 通道把 codec 头里的真实分辨率传回，用于登记群控会话。
	// 缓冲为 1，pumpVideo 无论成功读到分辨率还是失败退出都会发一个信号，
	// 保证主 goroutine 不会因 pumpVideo 提前 return 而永久卡死（锁泄漏）。
	resolution := make(chan [2]uint32, 1)
	go s.pumpVideo(tun.video, conn, resolution)

	// 等待 codec 头解析出分辨率后登记会话，供群控组引用。
	// 增加超时兜底：视频头在 8s 内没读到（网络抖动/僵尸 forward/scrcpy-server
	// 启动失败）就中止会话，defer 的 release 会释放观看锁，避免锁泄漏。
	var width, height uint32
	select {
	case sz := <-resolution:
		width, height = sz[0], sz[1]
	case <-r.Context().Done():
		return
	case <-time.After(8 * time.Second):
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": "视频流建立超时"})
		return
	}
	if width == 0 && height == 0 {
		// pumpVideo 读到错误信号（读头失败），直接结束会话，释放观看锁。
		return
	}
	s.registerSession(serial, tun.control, width, height)
	defer s.unregisterSession(serial)

	// 控制：WebSocket -> TCP（读浏览器控制事件，编码后写入）。
	s.pumpControl(conn, tun.control, serial)
}

// prepare 部署并启动 scrcpy-server，建立 adb forward，返回本地端口。
// cfg 允许按会话覆盖编码参数（群控降配）。无线调试下 daemon 可能意外重启
// 导致设备掉线，因此先确保 daemon 存活与设备在线。
func (s *Server) prepare(ctx context.Context, serial string, cfg ScrcpyConfig) (int, error) {
	if err := s.adb.EnsureServer(ctx); err != nil {
		return 0, err
	}
	if online, _ := s.adb.IsDeviceOnline(ctx, serial); !online {
		if err := s.adb.Reconnect(ctx, serial); err != nil {
			return 0, err
		}
	}

	if err := cfg.deploy(ctx, s.adb, serial); err != nil {
		return 0, err
	}
	if err := cfg.start(ctx, s.adb, serial); err != nil {
		return 0, err
	}

	// 为本次会话分配一个本地端口并建立 forward。
	port := s.allocatePort()
	if err := s.adb.Forward(ctx, serial, port, "localabstract:scrcpy"); err != nil {
		return 0, err
	}
	return port, nil
}

// allocatePort 为会话分配一个本地端口。会话短促，用时间戳+序列号生成即可，
// 冲突由 adb forward 报错兜底。
var portSeq struct {
	sync.Mutex
	n int
}

func (s *Server) allocatePort() int {
	portSeq.Lock()
	defer portSeq.Unlock()
	portSeq.n++
	// 从 45001 起递增，上限 55000 回绕。
	port := 45001 + (portSeq.n % 10000)
	return port
}

// cleanup 移除 forward，清理设备侧资源。
func (s *Server) cleanup(serial string, port int) {
	ctx := context.Background()
	if err := s.adb.RemoveForward(ctx, serial, port); err != nil {
		log.Printf("scrcpy remove forward failed serial=%s port=%d: %v", serial, port, err)
	}
}

// pumpVideo 从视频通道读取 codec 头与帧，转发到 WebSocket。
// resolution 通道（可空）在解析出 codec 头后回传真实分辨率，供群控登记用。
//
// 兼容 scrcpy-server v4 与 v5 两种视频流开头：
//   - v4：codec id(4) + width(4) + height(4)（或含 flags 的 16 字节扩展头）
//   - v5：codec id(4) + session packet(12 字节，最高位=1，含 width/height)
//
// 两者靠「codec id 之后的第一个 u32 最高位」区分：v5 session packet 最高位为 1，
// v4 的 width 是普通整数（< 2^31）。运行中 v5 还可能在旋转后插入 session packet，
// 需在帧循环里识别并跳过。
func (s *Server) pumpVideo(video interface{ Read([]byte) (int, error) }, conn *websocket.Conn, resolution chan<- [2]uint32) {
	codec, width, height, err := s.readVideoHeader(video)
	if err != nil {
		// 读头失败（网络抖动 / 僵尸 forward / scrcpy-server 启动失败）时，
		// 必须发一个「错误信号」唤醒主 goroutine，否则它会卡死在 select 上，
		// 导致观看锁永久泄漏（后续同 serial 永远报 device already being controlled）。
		log.Printf("scrcpy read video header failed: %v", err)
		if resolution != nil {
			select {
			case resolution <- [2]uint32{0, 0}:
			default:
			}
		}
		return
	}
	_ = conn.WriteJSON(map[string]any{
		"type": "codec", "codec": string(codec),
		"width": width, "height": height,
	})
	if resolution != nil {
		select {
		case resolution <- [2]uint32{width, height}:
		default:
		}
	}

	var frame []byte
	metaBuf := make([]byte, 12)
	for {
		if _, err := readFull(video, metaBuf); err != nil {
			return
		}
		// v5：旋转后可能在帧流中插入 session packet，识别并跳过（不转发）。
		if isSessionPacket(metaBuf) {
			if w, h, err := parseSessionPacket(metaBuf); err == nil {
				width, height = w, h
				// 分辨率变化时通知前端（可选，当前前端用 codec 头分辨率）。
				_ = conn.WriteJSON(map[string]any{
					"type": "codec", "codec": string(codec),
					"width": width, "height": height,
				})
			}
			continue
		}
		meta, err := parseFrameMeta(metaBuf)
		if err != nil {
			return
		}
		// 复用缓冲区：只有帧比上一次更大时才重新分配，避免每帧 malloc 造成 GC 压力。
		if meta.Size > uint32(cap(frame)) {
			frame = make([]byte, meta.Size)
		}
		frame = frame[:meta.Size]
		if _, err := readFull(video, frame); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return
		}
	}
}

// readVideoHeader 读取并解析视频流开头的 codec id 与分辨率，兼容 v4/v5。
// 返回 codec、width、height。v5 的分辨率来自 session packet，v4 来自 codec 头。
func (s *Server) readVideoHeader(video interface{ Read([]byte) (int, error) }) (Codec, uint32, uint32, error) {
	codecID := make([]byte, codecIDSize)
	if _, err := readFull(video, codecID); err != nil {
		return "", 0, 0, err
	}
	codecName := string(codecID)
	switch codecName {
	case "h264", "h265":
	default:
		return "", 0, 0, fmt.Errorf("unsupported codec %q", codecName)
	}
	codec := Codec(codecName)

	// 读接下来的 12 字节，判断是 v5 session packet 还是 v4 width/height。
	buf := make([]byte, 12)
	if _, err := readFull(video, buf); err != nil {
		return "", 0, 0, err
	}
	if isSessionPacket(buf) {
		// v5：session packet，宽高在其后 8 字节。
		width, height, err := parseSessionPacket(buf)
		return codec, width, height, err
	}
	// v4：buf 的前 8 字节是 width/height。
	width := binary.BigEndian.Uint32(buf[0:4])
	height := binary.BigEndian.Uint32(buf[4:8])
	if plausibleSize(width) && plausibleSize(height) {
		return codec, width, height, nil
	}
	return "", 0, 0, fmt.Errorf("unrecognized video header layout (width=%d height=%d)", width, height)
}

// pumpControl 从 WebSocket 读取控制事件，编码后写入控制通道。
// 当 serial 是某个群控组的主控时，事件会被广播给组内所有设备（含自身）。
func (s *Server) pumpControl(conn *websocket.Conn, control interface{ Write([]byte) (int, error) }, serial string) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// 群控：主控事件广播到组内所有设备（坐标按各自分辨率归一化）。
		if g := s.syncGroupOf(serial); g != nil && g.master == serial {
			s.broadcastControl(serial, data)
			continue
		}
		// 单设备：照旧只写本机控制通道。
		encoded, err := encodeControlMessage(data)
		if err != nil {
			log.Printf("scrcpy control encode failed: %v", err)
			continue
		}
		if _, err := control.Write(encoded); err != nil {
			return
		}
	}
}

// encodeControlMessage 解析浏览器发来的 JSON 控制事件，编码为 scrcpy 二进制。
func encodeControlMessage(data []byte) ([]byte, error) {
	var msg struct {
		Type    string  `json:"type"`
		Action  string  `json:"action"`
		X       uint32  `json:"x"`
		Y       uint32  `json:"y"`
		Width   uint32  `json:"width"`
		Height  uint32  `json:"height"`
		Key     uint32  `json:"key"`
		Text    string  `json:"text"`
		HScroll float64 `json:"hScroll"`
		VScroll float64 `json:"vScroll"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	switch msg.Type {
	case "touch":
		action := actionMove
		switch msg.Action {
		case "down":
			action = actionDown
		case "up":
			action = actionUp
		case "move":
			action = actionMove
		default:
			return nil, errInvalidAction
		}
		return encodeTouch(TouchAction{
			Action: action, X: msg.X, Y: msg.Y,
			Width: msg.Width, Height: msg.Height, Pointer: pointerIDMouse,
		})
	case "key":
		return encodeKeycode(KeyAction{Down: msg.Action != "up", Keycode: msg.Key}), nil
	case "text":
		return encodeText(msg.Text)
	case "scroll":
		return encodeScroll(ScrollAction{
			X: msg.X, Y: msg.Y, Width: msg.Width, Height: msg.Height,
			HScroll: msg.HScroll, VScroll: msg.VScroll,
		})
	case "back":
		return encodeKeyCombo(4), nil
	default:
		return nil, errInvalidType
	}
}

var (
	errInvalidAction = &controlError{"invalid action"}
	errInvalidType   = &controlError{"invalid control type"}
)

type controlError struct{ msg string }

func (e *controlError) Error() string { return e.msg }

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeError 输出错误响应。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readFull 从连接读取指定长度字节，兼容 net.Conn 的 Read 语义。
func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
