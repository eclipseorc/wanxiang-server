package websocket

import (
	"encoding/json"
	"testing"
)

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
