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

// 设备被同 ID 的新连接替换时，removeDevice 的 current=false 分支也必须释放
// watch 会话；否则 watch_sessions 会永久占用，之后所有观看请求都会失败。
func TestRemoveDeviceReleasesWatchWhenReplaced(t *testing.T) {
	hub := newTestHub(t)
	handler := NewHandler(hub)

	old := newClient(nil, "device", "dev-1")
	replacement := newClient(nil, "device", "dev-1")
	viewer := newClient(nil, "admin", "user-1")

	hub.mu.Lock()
	hub.devices["dev-1"] = replacement
	hub.mu.Unlock()
	hub.watchMu.Lock()
	hub.viewer, hub.sessionID, hub.deviceID = viewer, "session-1", "dev-1"
	hub.watchMu.Unlock()

	handler.removeDevice(old)

	hub.watchMu.Lock()
	sessionID, activeViewer := hub.sessionID, hub.viewer
	hub.watchMu.Unlock()
	if sessionID != "" || activeViewer != nil {
		t.Fatalf("watch session still active: session=%q viewer=%v", sessionID, activeViewer)
	}
	var remaining int
	if err := hub.store.DB.QueryRow(
		`SELECT COUNT(*) FROM watch_sessions WHERE session_id IS NOT NULL`,
	).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("database watch slot still reserved: %d", remaining)
	}
}

func TestEndWatchRecordsRealActor(t *testing.T) {
	hub := newTestHub(t)
	viewer := newClient(nil, "admin", "user-42")
	hub.watchMu.Lock()
	hub.viewer, hub.sessionID, hub.deviceID = viewer, "session-2", "dev-2"
	hub.watchMu.Unlock()

	hub.endWatch("session-2")

	var actor string
	if err := hub.store.DB.QueryRow(
		`SELECT actor_id FROM audit_logs WHERE action='watch-ended' AND detail='session-2'`,
	).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != "user-42" {
		t.Fatalf("audit actor = %q, want user-42", actor)
	}
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
