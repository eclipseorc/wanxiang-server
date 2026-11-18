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
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// Server 桥接浏览器与设备的 scrcpy 会话。
type Server struct {
	adb  *ADB
	cfg  ScrcpyConfig
	mu   sync.Mutex
	live map[string]bool // serial -> 是否正在被观看，防止同设备重复控制

	// 群控相关状态（见 sync.go）：由同一把 mu 保护。
	sessions map[string]*deviceSession // serial -> 活跃会话（control 通道 + 分辨率）
	groups   map[string]*syncGroup     // groupID -> 群控组
	inGroup  map[string]string         // serial -> groupID，一台设备只属于一个组
}

// NewServer 构造 scrcpy 服务。
func NewServer(adb *ADB, cfg ScrcpyConfig) *Server {
	return &Server{
		adb:      adb,
		cfg:      cfg,
		live:     make(map[string]bool),
		sessions: make(map[string]*deviceSession),
		groups:   make(map[string]*syncGroup),
		inGroup:  make(map[string]string),
	}
}

// ServeMux 返回服务路由。WS 端点不在此做 HTTP 鉴权，
// 由部署层（反向代理或前端）保证只有已登录管理员能访问。
func (s *Server) ServeMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/scrcpy/ws", s.handleWS)
	mux.HandleFunc("GET /api/scrcpy/sync", s.handleSyncWS)
	mux.HandleFunc("GET /api/scrcpy/devices", s.handleDevices)
	return mux
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
}

// handleWS 处理一次完整的远程观看会话。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	serial := strings.TrimSpace(r.URL.Query().Get("serial"))
	if serial == "" {
		writeError(w, http.StatusBadRequest, "serial is required")
		return
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

	// 同一设备同一时刻只允许一个观看会话。
	if !s.tryAcquire(serial) {
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": "device already being controlled"})
		return
	}
	defer s.release(serial)

	// 完成准备流程（部署 + 启动 + forward）。
	port, err := s.prepare(r.Context(), serial)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	defer s.cleanup(serial, port)

	// 建立视频 + 控制两条 TCP 通道。
	tun, err := establish(r.Context(), port)
	if err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	defer tun.close()

	log.Printf("scrcpy viewing serial=%s port=%d", serial, port)

	// 视频：TCP -> WebSocket（读 codec 头 + 帧，转发给浏览器）。
	// resolution 通道把 codec 头里的真实分辨率传回，用于登记群控会话。
	resolution := make(chan [2]uint32, 1)
	go s.pumpVideo(tun.video, conn, resolution)

	// 等待 codec 头解析出分辨率后登记会话，供群控组引用。
	var width, height uint32
	select {
	case sz := <-resolution:
		width, height = sz[0], sz[1]
	case <-r.Context().Done():
		return
	}
	s.registerSession(serial, tun.control, width, height)
	defer s.unregisterSession(serial)

	// 控制：WebSocket -> TCP（读浏览器控制事件，编码后写入）。
	s.pumpControl(conn, tun.control, serial)
}

// prepare 部署并启动 scrcpy-server，建立 adb forward，返回本地端口。
// 无线调试下 daemon 可能意外重启导致设备掉线，因此先确保 daemon 存活与设备在线。
func (s *Server) prepare(ctx context.Context, serial string) (int, error) {
	if err := s.adb.EnsureServer(ctx); err != nil {
		return 0, err
	}
	if online, _ := s.adb.IsDeviceOnline(ctx, serial); !online {
		if err := s.adb.Reconnect(ctx, serial); err != nil {
			return 0, err
		}
	}

	if err := s.cfg.deploy(ctx, s.adb, serial); err != nil {
		return 0, err
	}
	if err := s.cfg.start(ctx, s.adb, serial); err != nil {
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
func (s *Server) pumpVideo(video interface{ Read([]byte) (int, error) }, conn *websocket.Conn, resolution chan<- [2]uint32) {
	header := make([]byte, codecHeaderSizeExtended)
	n, err := readFull(video, header[:codecHeaderSize])
	if err != nil {
		return
	}
	if n == codecHeaderSize {
		if _, err := readFull(video, header[codecHeaderSize:codecHeaderSizeExtended]); err != nil {
			return
		}
		n = codecHeaderSizeExtended
	}
	codecHeader, err := parseCodecHeader(header[:n])
	if err != nil {
		return
	}
	_ = conn.WriteJSON(map[string]any{
		"type": "codec", "codec": string(codecHeader.Codec),
		"width": codecHeader.Width, "height": codecHeader.Height,
	})
	if resolution != nil {
		select {
		case resolution <- [2]uint32{codecHeader.Width, codecHeader.Height}:
		default:
		}
	}

	var frame []byte
	metaBuf := make([]byte, 12)
	for {
		if _, err := readFull(video, metaBuf); err != nil {
			return
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
