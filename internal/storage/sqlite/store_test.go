package sqlite

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := InitSchema(db); err != nil {
		t.Fatal(err)
	}
	return NewStore(db)
}

// OpenReady 会被两个服务进程调用；重复初始化必须安全。
func TestOpenReadyIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.db")
	db1, err := OpenReady(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := OpenReady(path)
	if err != nil {
		t.Fatalf("second OpenReady: %v", err)
	}
	defer db2.Close()
}

func TestCreateUserWritesReadableTimestamp(t *testing.T) {
	store := newTestStore(t)
	if err := store.CreateUser("u1", "admin", "hash", "admin"); err != nil {
		t.Fatal(err)
	}
	var createdAt string
	if err := store.DB.QueryRow(`SELECT created_at FROM users WHERE id=?`, "u1").Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse("2006-01-02 15:04:05", createdAt); err != nil {
		t.Fatalf("created_at = %q, want YYYY-MM-DD HH:MM:SS: %v", createdAt, err)
	}
}

func TestPurgeExpiredRemovesStaleCredentials(t *testing.T) {
	store := newTestStore(t)
	if err := store.CreateUser("u1", "admin", "hash", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("expired-session", "u1", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("valid-session", "u1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.NewTicket("expired-ticket", "admin", "u1", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.NewTicket("valid-ticket", "admin", "u1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := store.PurgeExpired(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateSession("expired-session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session still valid: %v", err)
	}
	if _, err := store.ValidateSession("valid-session"); err != nil {
		t.Fatalf("valid session was removed: %v", err)
	}
	if _, err := store.ConsumeTicket("expired-ticket"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired ticket still consumable: %v", err)
	}
	if _, err := store.ConsumeTicket("valid-ticket"); err != nil {
		t.Fatalf("valid ticket was removed: %v", err)
	}
}

func TestConsumeTicketIsSingleUse(t *testing.T) {
	store := newTestStore(t)
	if err := store.NewTicket("ticket-1", "device", "device-1", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.ConsumeTicket("ticket-1")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Type != "device" || ticket.SubjectID != "device-1" {
		t.Fatalf("unexpected ticket: %#v", ticket)
	}
	if _, err := store.ConsumeTicket("ticket-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replayed ticket was accepted: %v", err)
	}
}

func TestSetGroupAccessManageImpliesView(t *testing.T) {
	store := newTestStore(t)
	// group_access 有外键约束，必须先建立两个组。
	if err := store.CreateGroup("ug", "用户组"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateGroup("dg", "设备组"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupAccess("ug", "dg", false, true); err != nil {
		t.Fatal(err)
	}
	var view, manage int
	if err := store.DB.QueryRow(
		`SELECT can_view,can_manage FROM group_access WHERE user_group_id=? AND device_group_id=?`, "ug", "dg",
	).Scan(&view, &manage); err != nil {
		t.Fatal(err)
	}
	if view != 1 || manage != 1 {
		t.Fatalf("can_manage must force can_view, got view=%d manage=%d", view, manage)
	}
}

// UpsertDevice 幂等：同一 deviceId 重复登记不会新增行，只会更新型号。
func TestUpsertDeviceIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertDevice("device-a", "Model-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice("device-a", "Model-2"); err != nil {
		t.Fatal(err)
	}
	var count int
	var model string
	if err := store.DB.QueryRow(`SELECT COUNT(*), MAX(model) FROM devices WHERE id='device-a'`).Scan(&count, &model); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after upsert, got %d", count)
	}
	if model != "Model-2" {
		t.Fatalf("expected model to be updated to Model-2, got %q", model)
	}
}

// UpsertDevice 传入空型号（adb-ready 先行登记）时不应清空已有型号。
func TestUpsertDeviceKeepsModelWhenEmpty(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertDevice("device-b", "motorola moto e14"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice("device-b", ""); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := store.DB.QueryRow(`SELECT model FROM devices WHERE id='device-b'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "motorola moto e14" {
		t.Fatalf("expected model to be kept, got %q", model)
	}
}

// SetEnabled 对不存在的设备返回 ErrNotFound。
func TestSetEnabledNotFound(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetEnabled("no-such-device", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
