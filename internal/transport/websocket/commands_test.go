package websocket

import (
	"encoding/json"
	"testing"
)

// 测试 ping 成功返回 pong，并原样保留请求编号和客户端时间戳。
func TestDispatchPingReturnsPong(t *testing.T) {
	message := CommandMessage{
		Type:      commandType,
		Command:   commandPing,
		RequestID: "request-42",
		Data:      json.RawMessage(`{"sentAt":1731670000000}`),
	}

	response := dispatchCommand(message)
	if response.Type != commandType || response.Command != commandPong {
		t.Fatalf("response = %#v, want com/pong", response)
	}
	if response.RequestID != message.RequestID {
		t.Fatalf("requestId = %q, want %q", response.RequestID, message.RequestID)
	}
	var got pingData
	if err := json.Unmarshal(response.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.SentAt != 1731670000000 {
		t.Fatalf("sentAt = %d, want original timestamp", got.SentAt)
	}
}

// 测试服务端拒绝未列入白名单的命令，并返回可关联的错误响应。
func TestDispatchRejectsUnknownCommand(t *testing.T) {
	response := dispatchCommand(CommandMessage{
		Type:      commandType,
		Command:   "run-arbitrary-action",
		RequestID: "request-43",
	})
	if response.Command != commandError || response.RequestID != "request-43" {
		t.Fatalf("response = %#v, want correlated command error", response)
	}
	var got commandErrorData
	if err := json.Unmarshal(response.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != commandUnsupported {
		t.Fatalf("error code = %q, want %q", got.Code, commandUnsupported)
	}
}

// 测试 ping 的 data 格式不正确时会返回 invalid_command，而不是崩溃。
func TestDispatchRejectsMalformedPing(t *testing.T) {
	response := dispatchCommand(CommandMessage{
		Type:    commandType,
		Command: commandPing,
		Data:    json.RawMessage(`{"sentAt":"not-a-number"}`),
	})
	if response.Command != commandError {
		t.Fatalf("response command = %q, want error", response.Command)
	}
}

// 错误响应的命令名必须保持为 "error"，且带上可关联的 requestId 与错误码。
func TestCommandErrorEnvelope(t *testing.T) {
	response := newCommandError("req-99", commandInvalid, "bad payload")
	if response.Type != commandType || response.Command != commandError {
		t.Fatalf("response = %#v, want com/error", response)
	}
	if response.RequestID != "req-99" {
		t.Fatalf("requestId = %q, want req-99", response.RequestID)
	}
	var got commandErrorData
	if err := json.Unmarshal(response.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != commandInvalid || got.Message != "bad payload" {
		t.Fatalf("error data = %#v", got)
	}
}

// 缺少外层 type 的命令不能被当作合法命令执行。
func TestDispatchRejectsMissingType(t *testing.T) {
	response := dispatchCommand(CommandMessage{
		Command:   commandPing,
		RequestID: "req-100",
		Data:      json.RawMessage(`{"sentAt":1731670000000}`),
	})
	if response.Command != commandError {
		t.Fatalf("response command = %q, want error", response.Command)
	}
	if response.RequestID != "req-100" {
		t.Fatalf("requestId = %q, want req-100", response.RequestID)
	}
	var got commandErrorData
	if err := json.Unmarshal(response.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != commandInvalid {
		t.Fatalf("error code = %q, want invalid_command", got.Code)
	}
}
