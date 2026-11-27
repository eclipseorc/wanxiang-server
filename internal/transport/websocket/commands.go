package websocket

import (
	"encoding/json"
	"log"
	"net"
	"strings"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// 本文件是 WebSocket 层的「命令总注册表」：
//   - 所有设备端入站消息统一注册在 deviceRoutes
//   - 所有管理端入站消息统一注册在 adminRoutes
//   - 所有 type="com" 应用层命令统一注册在 comHandlers
//
// 新增命令只需在此文件注册一条映射并实现对应处理函数，无需改动 server.go。

const (
	// commandType 是应用层命令的固定外层类型，避免命令与普通事件混淆。
	commandType = "com"
	// 以下常量是当前已支持的心跳命令及标准错误命令名称。
	commandPing        = "ping"
	commandPong        = "pong"
	commandError       = "error"
	commandUnsupported = "unsupported_command"
	commandInvalid     = "invalid_command"
)

// CommandMessage 是所有应用层命令（type="com"）共用的 JSON 信封。
// requestId 用于将响应关联回请求；data 的具体结构由各命令自行定义。
type CommandMessage struct {
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	RequestID string          `json:"requestId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// pingData 是 ping/pong 命令中回传的客户端发送时间戳。
type pingData struct {
	SentAt int64 `json:"sentAt"`
}

// commandErrorData 是命令失败时放在 data 字段中的错误代码和说明。
type commandErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---------- 1. 设备端消息注册表（/ws/device） ----------

type deviceRouteFunc func(h *Handler, c *client, msg map[string]any)

// deviceRoutes 列出全部设备端可接收的消息类型。
// 新增设备端消息：在此表增加一行，并在下方补充对应处理方法即可。
var deviceRoutes = map[string]deviceRouteFunc{
	commandType:          (*Handler).handleDeviceCommand,  // com 信封命令（ping 等）
	"join":               (*Handler).handleDeviceJoin,     // 设备加入确认
	"out":                (*Handler).handleDeviceOut,      // 设备主动退出
	"heartbeat":          (*Handler).handleHeartbeat,      // 周期心跳 + 电量 + 局域网 IP 变更同步
	"adb-ready":          (*Handler).handleAdbReady,       // ADB 就绪上报（触发 adb connect）
	"adb-pair-request":   (*Handler).handleAdbPairRequest, // 无障碍读码后请求服务端 adb pair + connect
	"ui-tree-result":     (*Handler).handleUITreeResult,   // 50ms 无障碍控件树回传
	"pull-device-script": (*Handler).handlePullScript,     // 拉取本机型号对应的自动配对脚本
	"status":             (*Handler).handleDeviceStatus,   // 通用状态透传广播
	"command-result":     (*Handler).handleCommandResult,  // 命令执行回执转发
}

// ---------- 2. 管理端消息注册表（/ws/admin） ----------

type adminRouteFunc func(h *Handler, c *client, user sqlite.User, msg map[string]any)

// adminRoutes 列出全部管理端可接收的消息类型。
// 新增管理端消息：在此表增加一行，并在下方补充对应处理方法即可（注意通过 Store.CanAccess 校验权限）。
var adminRoutes = map[string]adminRouteFunc{
	commandType:      (*Handler).handleAdminCommand,  // com 信封命令（ping 等）
	"device-control": (*Handler).handleDeviceControl, // 启用 / 断开设备
	"ping":           (*Handler).handleLegacyPing,    // 兼容旧版管理端心跳
}

// ---------- 3. 应用层 com 命令白名单注册表（type="com"） ----------

// comContext 携带 com 命令执行所需的完整上下文，便于扩展需查库或转发设备的业务命令。
type comContext struct {
	Handler *Handler
	Client  *client
	User    *sqlite.User // 仅管理端连接非空；设备端或纯单测调用时为 nil
}

type comHandlerFunc func(ctx comContext, msg CommandMessage) CommandMessage

// comHandlers 列出全部 type="com" 应用层命令白名单；未知命令统一返回 unsupported_command。
// 新增 com 命令：在此表注册命令名与处理函数即可。
var comHandlers = map[string]comHandlerFunc{
	commandPing: handleComPing, // 应用层心跳探测（返回 pong 与原 sentAt）
}

// ---------- 统一分发入口 ----------

// dispatchCommand 按白名单分发无状态 com 命令（保留原签名以兼容既有协议单测）。
func dispatchCommand(message CommandMessage) CommandMessage {
	return dispatchCommandWithContext(comContext{}, message)
}

func dispatchCommandWithContext(ctx comContext, message CommandMessage) CommandMessage {
	if message.Type != commandType || message.Command == "" {
		return newCommandError(message.RequestID, commandInvalid, "type and command are required")
	}
	fn, ok := comHandlers[message.Command]
	if !ok {
		return newCommandError(message.RequestID, commandUnsupported, "unsupported command")
	}
	return fn(ctx, message)
}

func handleComPing(_ comContext, message CommandMessage) CommandMessage {
	var data pingData
	if len(message.Data) == 0 || json.Unmarshal(message.Data, &data) != nil {
		return newCommandError(message.RequestID, commandInvalid, "ping data must contain sentAt")
	}
	payload, _ := json.Marshal(data)
	return CommandMessage{
		Type:      commandType,
		Command:   commandPong,
		RequestID: message.RequestID,
		Data:      payload,
	}
}

// newCommandError 创建保留原 requestId 的标准错误响应，便于客户端匹配失败请求。
// 构造函数名不能与命令名常量 commandError 相同：Go 不允许同一包内
// 常量与函数重名，且响应里的 Command 字段必须是字符串 "error"。
func newCommandError(requestID, code, message string) CommandMessage {
	data, _ := json.Marshal(commandErrorData{Code: code, Message: message})
	return CommandMessage{
		Type:      commandType,
		Command:   commandError,
		RequestID: requestID,
		Data:      data,
	}
}

// ---------- 设备端 / 管理端路由处理方法 ----------

func (h *Handler) handleDeviceCommand(c *client, raw map[string]any) {
	h.handleCommandWithUser(c, nil, raw)
}

func (h *Handler) handleAdminCommand(c *client, user sqlite.User, raw map[string]any) {
	u := user
	h.handleCommandWithUser(c, &u, raw)
}

func (h *Handler) handleCommandWithUser(c *client, user *sqlite.User, raw map[string]any) {
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
	response := dispatchCommandWithContext(comContext{Handler: h, Client: c, User: user}, message)
	if err := c.send(response); err != nil {
		log.Printf("websocket command response failed kind=%s id=%s command=%s: %v", c.kind, c.id, response.Command, err)
	}
}

func (h *Handler) handleDeviceJoin(c *client, _ map[string]any) {
	_ = c.send(map[string]string{"type": "joined", "deviceId": c.id})
}

func (h *Handler) handleDeviceOut(c *client, _ map[string]any) {
	_ = c.send(map[string]string{"type": "out-ack"})
	c.close()
}

func (h *Handler) handleHeartbeat(c *client, message map[string]any) {
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
	// 记录设备上报的本机 IP；若局域网 IP 变化，立即同步数据库并异步重连新 IP。
	if ip, _ := message["ip"].(string); ip != "" {
		ip = strings.TrimSpace(ip)
		if len(ip) > 64 {
			ip = ip[:64]
		}
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			h.hub.mu.Lock()
			prevIP := h.hub.deviceIPs[c.id]
			h.hub.deviceIPs[c.id] = ip
			h.hub.mu.Unlock()
			if prevIP != ip {
				h.syncDeviceIPChange(c.id, ip)
			}
		}
	}
	if err := h.hub.store.UpdateDeviceState(c.id, battery, rtt, status); err != nil {
		log.Printf("device heartbeat state update failed device_id=%s: %v", c.id, err)
	}
	_ = c.send(map[string]any{"type": "heartbeat-ack", "sentAt": message["sentAt"]})
	h.hub.broadcastDevice(c.id, "device-state", map[string]any{
		"battery": battery, "rttMs": rtt, "status": status, "lastSeen": time.Now().Unix(),
	})
}

func (h *Handler) handleUITreeResult(_ *client, message map[string]any) {
	reqID, _ := message["reqId"].(string)
	if reqID == "" {
		return
	}
	h.hub.mu.RLock()
	ch := h.hub.uiTreeWaiters[reqID]
	h.hub.mu.RUnlock()
	if ch == nil {
		return
	}
	okVal, _ := message["ok"].(bool)
	if !okVal {
		select {
		case ch <- nil:
		default:
		}
		return
	}
	rawNodes, err := json.Marshal(message["nodes"])
	if err != nil {
		rawNodes = nil
	}
	select {
	case ch <- rawNodes:
	default:
	}
}

func (h *Handler) handlePullScript(c *client, _ map[string]any) {
	h.sendDeviceScript(c)
}

func (h *Handler) handleDeviceStatus(c *client, message map[string]any) {
	h.hub.broadcastDevice(c.id, "device-state", boundedPayload(message["data"]))
}

func (h *Handler) handleCommandResult(c *client, message map[string]any) {
	// 只挑必要字段转发，避免设备提交的任意字段被放大广播给所有管理端。
	payload := make(map[string]any, 4)
	for _, key := range []string{"commandId", "requestId", "ok", "result"} {
		if value, ok := message[key]; ok {
			payload[key] = boundedPayload(value)
		}
	}
	h.hub.broadcastDevice(c.id, "command-result", payload)
}

func (h *Handler) handleLegacyPing(c *client, _ sqlite.User, message map[string]any) {
	_ = c.send(map[string]any{"type": "pong", "sentAt": message["sentAt"]})
}
