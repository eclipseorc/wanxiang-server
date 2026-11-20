package websocket

import (
	"context"
	"testing"
)

// fakeConnector 是一个可编程的 AdbReadyConnector，用于验证 handleAdbReady 的转交逻辑。
type fakeConnector struct {
	called    bool
	gotIP     string
	gotPort   int
	connected bool
}

func (f *fakeConnector) ConnectAfterReady(_ context.Context, _, ip string, port int) (bool, error) {
	f.called = true
	f.gotIP = ip
	f.gotPort = port
	return f.connected, nil
}

// TestHandleAdbReadyNoConnector 验证未注入 connector 时收到 adb-ready 不 panic、
// 不执行 connect，只回 ack。
func TestHandleAdbReadyNoConnector(t *testing.T) {
	hub := newTestHub(t)
	handler := NewHandler(hub)

	// client.conn 为 nil，send 会安全返回错误，不会 panic。
	c := newClient(nil, "device", "dev-1")
	handler.handleAdbReady(c, map[string]any{
		"lan_ip":   "192.168.1.50",
		"adb_port": float64(5555),
	})

	if hub.connector != nil {
		t.Fatal("hub.connector should remain nil in this test")
	}
}

// TestHandleAdbReadyDelegatesToConnector 验证注入 connector 后 adb-ready 正确解析
// IP/端口并转交给 connector。
func TestHandleAdbReadyDelegatesToConnector(t *testing.T) {
	hub := newTestHub(t)
	fc := &fakeConnector{connected: true}
	hub.WithAdbConnector(fc)
	handler := NewHandler(hub)

	c := newClient(nil, "device", "dev-1")
	handler.handleAdbReady(c, map[string]any{
		"lan_ip":   "192.168.1.60",
		"adb_port": float64(5555),
	})

	if !fc.called {
		t.Fatal("connector was not called")
	}
	if fc.gotIP != "192.168.1.60" || fc.gotPort != 5555 {
		t.Fatalf("connector got ip=%q port=%d, want 192.168.1.60:5555", fc.gotIP, fc.gotPort)
	}
}

// TestHandleAdbReadyEmptyIP 验证 lan_ip 为空时不调用 connector，直接回 ack。
func TestHandleAdbReadyEmptyIP(t *testing.T) {
	hub := newTestHub(t)
	fc := &fakeConnector{}
	hub.WithAdbConnector(fc)
	handler := NewHandler(hub)

	c := newClient(nil, "device", "dev-1")
	handler.handleAdbReady(c, map[string]any{
		"lan_ip":   "  ",
		"adb_port": float64(5555),
	})

	if fc.called {
		t.Fatal("connector should not be called for empty lan_ip")
	}
}
