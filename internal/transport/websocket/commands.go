package websocket

import "encoding/json"

const (
	commandType      = "com"
	commandPing      = "ping"
	commandPong      = "pong"
	commandError     = "error"
	commandUnsupported = "unsupported_command"
	commandInvalid   = "invalid_command"
)

// CommandMessage is the common envelope for application-level WebSocket commands.
type CommandMessage struct {
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	RequestID string          `json:"requestId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type pingData struct {
	SentAt int64 `json:"sentAt"`
}

type commandErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// dispatchCommand processes supported commands without coupling protocol parsing
// to the WebSocket connection lifecycle.
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

func commandError(requestID, code, message string) CommandMessage {
	data, _ := json.Marshal(commandErrorData{Code: code, Message: message})
	return CommandMessage{
		Type:      commandType,
		Command:   commandError,
		RequestID: requestID,
		Data:      data,
	}
}
