package logic

import (
	"context"
	"testing"
)

// TestAdbConnectorWithoutAdb 验证未配置 adb 时 ConnectAfterReady 仍会登记设备、
// 落库 AdbIP/AdbPort 与在线状态，但返回未连接、不报错。
func TestAdbConnectorWithoutAdb(t *testing.T) {
	svc := newTestService(t)
	conn := NewAdbConnector(svc, nil)

	connected, err := conn.ConnectAfterReady(context.Background(), "dev-adb-ready-1", "192.168.1.106", 5555)
	if err != nil {
		t.Fatalf("ConnectAfterReady returned error: %v", err)
	}
	if connected {
		t.Fatalf("connected = true, want false when adb not configured")
	}

	dev, err := svc.Store.DeviceByID("dev-adb-ready-1")
	if err != nil {
		t.Fatal(err)
	}
	if dev.AdbIP != "192.168.1.106" || dev.AdbPort != 5555 {
		t.Fatalf("AdbIP/AdbPort not persisted: %s:%d", dev.AdbIP, dev.AdbPort)
	}
	if !dev.Online {
		t.Fatalf("device should be marked online")
	}
}

// TestAdbConnectorDefaultPort 验证 port<=0 时默认回落到 5555。
func TestAdbConnectorDefaultPort(t *testing.T) {
	svc := newTestService(t)
	conn := NewAdbConnector(svc, nil)

	if _, err := conn.ConnectAfterReady(context.Background(), "dev-adb-ready-2", "192.168.1.107", 0); err != nil {
		t.Fatalf("ConnectAfterReady returned error: %v", err)
	}

	dev, err := svc.Store.DeviceByID("dev-adb-ready-2")
	if err != nil {
		t.Fatal(err)
	}
	if dev.AdbPort != 5555 {
		t.Fatalf("AdbPort = %d, want 5555 (default)", dev.AdbPort)
	}
}

// TestAdbConnectorMissingFields 验证 device_id 或 ip 缺失时返回错误。
func TestAdbConnectorMissingFields(t *testing.T) {
	svc := newTestService(t)
	conn := NewAdbConnector(svc, nil)

	if _, err := conn.ConnectAfterReady(context.Background(), "", "192.168.1.108", 5555); err == nil {
		t.Fatalf("expected error for empty device_id")
	}
	if _, err := conn.ConnectAfterReady(context.Background(), "dev-x", "", 5555); err == nil {
		t.Fatalf("expected error for empty ip")
	}
}
