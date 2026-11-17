// sync.go 在单设备观看链路之上叠加「群控组」，实现一次操作同步到多台设备。
//
// 设计（方案 α）：不改动现有 /api/scrcpy/ws 的单设备会话建立与视频转发，
// 只新增一层「注册表 + 事件广播」：
//
//   - 每台设备的活跃会话（control 通道 + 真实分辨率）在 pumpVideo 读到 codec 头后
//     登记进 Server.sessions。
//   - 前端通过独立的 /api/scrcpy/sync 端点把若干台设备编成一个群控组。
//   - 当主控设备收到触摸/滚动事件时，按主控分辨率归一化坐标，再按各从机分辨率
//     换算后写入各从机的 control 通道；按键/文本/返回等无坐标事件原样广播。
//
// 群控组是「临时编排」，与设备分组（授权边界）无关；成员必须已建立独立会话。
package scrcpy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
)

// deviceSession 是一台设备活跃会话中，群控需要的最小信息。
// 它与现有观看会话的 video/control 连接是同一份，不额外建连。
type deviceSession struct {
	serial  string
	control net.Conn   // 控制通道，用于写入控制事件
	width   uint32     // 设备真实分辨率宽
	height  uint32     // 设备真实分辨率高
}

// syncGroup 是一个群控组：一个主控 + 若干从机。
type syncGroup struct {
	id      string
	master  string                    // 主控 serial
	members map[string]*deviceSession // 组内所有设备（含主控）
}

// registerSession 把一台设备的活跃会话登记进注册表。
// width/height 是 codec 头里的设备真实分辨率。
func (s *Server) registerSession(serial string, control net.Conn, width, height uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]*deviceSession)
	}
	s.sessions[serial] = &deviceSession{serial: serial, control: control, width: width, height: height}
}

// unregisterSession 移除一台设备的会话登记；若它在某个群控组里，一并摘除。
func (s *Server) unregisterSession(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, serial)

	// 从它所在的群控组中移除，并在组变空时解散。
	for gid, g := range s.groups {
		if _, ok := g.members[serial]; ok {
			delete(g.members, serial)
			if g.master == serial {
				g.master = ""
			}
			if len(g.members) == 0 {
				delete(s.groups, gid)
			}
		}
	}
	delete(s.inGroup, serial)
}

// syncGroupOf 返回 serial 所在的群控组；不在任何组时返回 nil。
func (s *Server) syncGroupOf(serial string) *syncGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	gid, ok := s.inGroup[serial]
	if !ok {
		return nil
	}
	return s.groups[gid]
}

// broadcastControl 把主控收到的一个控制事件广播给群控组内所有设备。
//
// evt 中的坐标是「主控设备分辨率」下的像素；对每台设备（含主控自身）都按
// 各自真实分辨率做比例归一化换算，保证不同分辨率的设备落在屏幕同一相对位置。
// 事件类型里的 key/text/back 无坐标，原样编码即可，不受分辨率影响。
func (s *Server) broadcastControl(serial string, data []byte) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return
	}
	// 只对触摸与滚动做坐标归一化；其余事件（key/text/back）走原始编码。
	switch probe.Type {
	case "touch":
		s.broadcastTouch(serial, data)
	case "scroll":
		s.broadcastScroll(serial, data)
	default:
		s.broadcastRaw(serial, data)
	}
}

// broadcastTouch 归一化并广播一次触摸事件。
func (s *Server) broadcastTouch(serial string, data []byte) {
	var msg struct {
		Action string `json:"action"`
		X      uint32 `json:"x"`
		Y      uint32 `json:"y"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	action := actionMove
	switch msg.Action {
	case "down":
		action = actionDown
	case "up":
		action = actionUp
	case "move":
		action = actionMove
	default:
		return
	}

	g := s.syncGroupOf(serial)
	if g == nil {
		return
	}
	master := g.members[g.master]
	if master == nil {
		return
	}
	// 归一化：主控分辨率 → [0,1)。
	nx := float64(msg.X) / float64(master.width)
	ny := float64(msg.Y) / float64(master.height)

	for _, d := range g.members {
		x := uint32(nx * float64(d.width))
		y := uint32(ny * float64(d.height))
		// 边界兜底：归一化后可能因浮点取整触到边界，收敛到 [0, width-1]。
		if x >= d.width {
			x = d.width - 1
		}
		if y >= d.height {
			y = d.height - 1
		}
		frame, err := encodeTouch(TouchAction{
			Action: action, X: x, Y: y,
			Width: d.width, Height: d.height, Pointer: pointerIDMouse,
		})
		if err != nil {
			log.Printf("scrcpy sync touch encode failed serial=%s: %v", d.serial, err)
			continue
		}
		if _, err := d.control.Write(frame); err != nil {
			log.Printf("scrcpy sync touch write failed serial=%s: %v", d.serial, err)
		}
	}
}

// broadcastScroll 归一化并广播一次滚动事件。滚动量本身是 [-1,1] 归一化值，
// 无需按分辨率换算，只有 X/Y 坐标需要。
func (s *Server) broadcastScroll(serial string, data []byte) {
	var msg struct {
		X       uint32  `json:"x"`
		Y       uint32  `json:"y"`
		HScroll float64 `json:"hScroll"`
		VScroll float64 `json:"vScroll"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	g := s.syncGroupOf(serial)
	if g == nil {
		return
	}
	master := g.members[g.master]
	if master == nil {
		return
	}
	nx := float64(msg.X) / float64(master.width)
	ny := float64(msg.Y) / float64(master.height)

	for _, d := range g.members {
		x := uint32(nx * float64(d.width))
		y := uint32(ny * float64(d.height))
		if x >= d.width {
			x = d.width - 1
		}
		if y >= d.height {
			y = d.height - 1
		}
		frame, err := encodeScroll(ScrollAction{
			X: x, Y: y, Width: d.width, Height: d.height,
			HScroll: msg.HScroll, VScroll: msg.VScroll,
		})
		if err != nil {
			log.Printf("scrcpy sync scroll encode failed serial=%s: %v", d.serial, err)
			continue
		}
		if _, err := d.control.Write(frame); err != nil {
			log.Printf("scrcpy sync scroll write failed serial=%s: %v", d.serial, err)
		}
	}
}

// broadcastRaw 广播无坐标事件（key/text/back），每台设备用完全相同的编码帧。
func (s *Server) broadcastRaw(serial string, data []byte) {
	encoded, err := encodeControlMessage(data)
	if err != nil {
		log.Printf("scrcpy sync raw encode failed: %v", err)
		return
	}
	g := s.syncGroupOf(serial)
	if g == nil {
		return
	}
	for _, d := range g.members {
		if _, err := d.control.Write(encoded); err != nil {
			log.Printf("scrcpy sync raw write failed serial=%s: %v", d.serial, err)
		}
	}
}

// handleDevices 返回当前 adb 中已完成配对+连接（device 状态）的设备 serial 列表，
// 作为「可加入群控组」的候选设备。serial 即无线设备的 ip:port。
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if s.adb == nil {
		writeError(w, http.StatusServiceUnavailable, "adb not available")
		return
	}
	serials, err := s.adb.OnlineDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"serials": serials})
}

// handleSyncWS 处理群控组管理端点 /api/scrcpy/sync 的消息。
func (s *Server) handleSyncWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		s.handleSyncMessage(conn, data)
	}
}

// handleSyncMessage 解析并执行一条群控管理消息。
// 各 sync* 函数返回结果结构，由本函数统一写回 WS。
func (s *Server) handleSyncMessage(conn *websocket.Conn, data []byte) {
	var msg struct {
		Type    string   `json:"type"`
		GroupID string   `json:"groupId"`
		Serials []string `json:"serials"`
		Master  string   `json:"master"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		_ = conn.WriteJSON(map[string]string{"type": "sync-error", "message": "bad message"})
		return
	}

	var resp any
	switch msg.Type {
	case "sync-create":
		// 先校验成员是否都已完成配对+连接（adb devices 的 device 状态）。
		// 校验在锁外执行，因为 adb 命令有网络/进程开销。
		if err := s.requireOnline(msg.Serials); err != nil {
			resp = syncError(err.Error())
			break
		}
		resp = s.syncCreate(msg.GroupID, msg.Serials, msg.Master)
	case "sync-set-master":
		resp = s.syncSetMaster(msg.GroupID, msg.Master)
	case "sync-destroy":
		resp = s.syncDestroy(msg.GroupID)
	default:
		resp = syncError("unknown sync type")
	}
	if err := conn.WriteJSON(resp); err != nil {
		log.Printf("scrcpy sync write response failed: %v", err)
	}
}

// requireOnline 校验 serials 全部处于 adb device（已配对+连接）状态。
// 任一不在线则返回错误；adb 不可用时也拒绝，避免绕过配对约束。
func (s *Server) requireOnline(serials []string) error {
	if s.adb == nil {
		return errAdbUnavailable
	}
	online, err := s.adb.OnlineDevices(context.Background())
	if err != nil {
		return fmt.Errorf("query adb devices: %w", err)
	}
	if missing := missingSerials(serials, online); len(missing) > 0 {
		return fmt.Errorf("device not connected: %s", missing[0])
	}
	return nil
}

// missingSerials 返回 serials 中不在 online 集合里的项，保持原顺序。
func missingSerials(serials, online []string) []string {
	set := make(map[string]bool, len(online))
	for _, s := range online {
		set[s] = true
	}
	var missing []string
	for _, s := range serials {
		if !set[s] {
			missing = append(missing, s)
		}
	}
	return missing
}

var errAdbUnavailable = fmt.Errorf("adb not available")

// syncError 构造一个群控错误响应。
func syncError(message string) map[string]any {
	return map[string]any{"type": "sync-error", "message": message}
}

// syncCreate 用一组已建立会话的设备创建一个群控组。
// 组内每台设备都必须已登记会话；主控缺省取列表第一个。返回响应消息。
func (s *Server) syncCreate(groupID string, serials []string, master string) map[string]any {
	if groupID == "" || len(serials) < 1 {
		return syncError("groupId and serials required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.groups == nil {
		s.groups = make(map[string]*syncGroup)
	}
	if s.inGroup == nil {
		s.inGroup = make(map[string]string)
	}
	if _, exists := s.groups[groupID]; exists {
		return syncError("group already exists")
	}

	g := &syncGroup{id: groupID, members: make(map[string]*deviceSession)}
	for _, serial := range serials {
		// 一台设备同一时刻只能在一个群控组里，避免控制通道被并发写乱。
		if _, in := s.inGroup[serial]; in {
			return syncError("device already in a group: " + serial)
		}
		ds, ok := s.sessions[serial]
		if !ok {
			return syncError("device has no active session: " + serial)
		}
		g.members[serial] = ds
	}

	if master != "" {
		if _, ok := g.members[master]; !ok {
			return syncError("master not in group")
		}
		g.master = master
	} else {
		g.master = serials[0]
	}

	s.groups[groupID] = g
	for serial := range g.members {
		s.inGroup[serial] = groupID
	}
	log.Printf("scrcpy sync group created id=%s master=%s members=%d", groupID, g.master, len(g.members))
	return map[string]any{
		"type": "sync-created", "groupId": groupID, "master": g.master,
		"members": serials,
	}
}

// syncSetMaster 更换群控组的主控设备。返回响应消息。
func (s *Server) syncSetMaster(groupID, master string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupID]
	if !ok {
		return syncError("group not found")
	}
	if _, ok := g.members[master]; !ok {
		return syncError("master not in group")
	}
	g.master = master
	return map[string]any{"type": "sync-master-set", "groupId": groupID, "master": master}
}

// syncDestroy 解散群控组。返回响应消息。
func (s *Server) syncDestroy(groupID string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[groupID]
	if !ok {
		return syncError("group not found")
	}
	for serial := range g.members {
		delete(s.inGroup, serial)
	}
	delete(s.groups, groupID)
	log.Printf("scrcpy sync group destroyed id=%s", groupID)
	return map[string]any{"type": "sync-destroyed", "groupId": groupID}
}
