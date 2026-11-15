package websocket

import "encoding/json"

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

// CommandMessage 是所有应用层命令共用的 JSON 信封。
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

// dispatchCommand 按白名单分发命令；未知命令不会被执行，而是返回统一错误。
func dispatchCommand(message CommandMessage) CommandMessage {
	if message.Type != commandType || message.Command == "" {
		return commandError(message.RequestID, commandInvalid, "type and command are required")
	}

	switch message.Command {
	case commandPing:
		var data pingData
		if len(message.Data) == 0 || json.Unmarshal(message.Data, &data) != nil {
			return commandError(message.RequestID, commandInvalid, "ping data must contain sentAt")
		}
		payload, _ := json.Marshal(data)
		return CommandMessage{
			Type:      commandType,
			Command:   commandPong,
			RequestID: message.RequestID,
			Data:      payload,
		}
	default:
		return commandError(message.RequestID, commandUnsupported, "unsupported command")
	}
}

// commandError 创建保留原 requestId 的标准错误响应，便于客户端匹配失败请求。
func commandError(requestID, code, message string) CommandMessage {
	data, _ := json.Marshal(commandErrorData{Code: code, Message: message})
	return CommandMessage{
		Type:      commandType,
		Command:   commandError,
		RequestID: requestID,
		Data:      data,
	}
}
