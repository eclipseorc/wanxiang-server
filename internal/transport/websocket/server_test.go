package websocket

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.InitSchema(db); err != nil {
		t.Fatal(err)
	}
	return NewHub(sqlite.NewStore(db))
}

func TestBoundedPayloadTruncatesBroadcastData(t *testing.T) {
	long := strings.Repeat("x", maxPayloadBytes+100)
	got := boundedPayload(long)
	text, ok := got.(string)
	if !ok || len(text) != maxPayloadBytes {
		t.Fatalf("string was not truncated to %d bytes", maxPayloadBytes)
	}

	fields := make(map[string]any)
	for i := 0; i < 40; i++ {
		fields[string(rune('a'+i%26))+string(rune('0'+i/26))] = long
	}
	gotMap, ok := boundedPayload(fields).(map[string]any)
	if !ok {
		t.Fatal("expected a map result")
	}
	if len(gotMap) > 32 {
		t.Fatalf("map was not capped: %d fields", len(gotMap))
	}
	for key, value := range gotMap {
		if text, ok := value.(string); ok && len(text) > maxPayloadBytes {
			t.Fatalf("field %q was not truncated", key)
		}
	}
}

func TestMessageRateLimitAllowsNormalTraffic(t *testing.T) {
	c := &client{}
	now := time.Now()
	for i := 0; i < maxMessagesPerSecond; i++ {
		if !c.allowMessage(now) {
			t.Fatalf("message %d was rejected within the limit", i+1)
		}
	}
	if c.allowMessage(now) {
		t.Fatal("message over the per-second limit was accepted")
	}
	if !c.allowMessage(now.Add(time.Second)) {
		t.Fatal("new window did not reset the budget")
	}
}

// 设备控制消息必须校验 can_manage；普通用户无权下发，且不会改写 enabled 状态。
func TestDeviceControlRequiresManagePermission(t *testing.T) {
	hub := newTestHub(t)
	handler := NewHandler(hub)

	// 注册一台设备与一个普通用户，但用户未获得任何分组授权。
	if err := hub.store.UpsertDevice("dev-1", "Model-X"); err != nil {
		t.Fatal(err)
	}
	if err := hub.store.CreateUser("user-1", "user-1", "hash", "user"); err != nil {
		t.Fatal(err)
	}
	viewer := newClient(nil, "admin", "user-1")

	handler.handleDeviceControl(viewer, sqlite.User{ID: "user-1", Role: "user"}, map[string]any{
		"deviceId": "dev-1", "action": "disconnect",
	})

	enabled, err := hub.store.DeviceEnabled("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("device was disabled despite missing can_manage permission")
	}
}

// 管理员下发 disconnect 会写库并把设备置为停用。
func TestDeviceControlDisconnectSetsEnabled(t *testing.T) {
	hub := newTestHub(t)
	handler := NewHandler(hub)

	if err := hub.store.UpsertDevice("dev-1", "Model-X"); err != nil {
		t.Fatal(err)
	}
	// 管理员必须真实存在于数据库，CanAccess 才会命中 admin 分支。
	if err := hub.store.CreateUser("admin-1", "admin-1", "hash", "admin"); err != nil {
		t.Fatal(err)
	}
	viewer := newClient(nil, "admin", "admin-1")

	handler.handleDeviceControl(viewer, sqlite.User{ID: "admin-1", Role: "admin"}, map[string]any{
		"deviceId": "dev-1", "action": "disconnect",
	})

	enabled, err := hub.store.DeviceEnabled("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("device was not disabled after disconnect control")
	}
}
