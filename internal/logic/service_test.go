package logic

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.InitSchema(db); err != nil {
		t.Fatal(err)
	}
	return New(sqlite.NewStore(db))
}

func TestEnsureAdminCreatesAccountOnce(t *testing.T) {
	svc := newTestService(t)
	const password = "a-very-strong-admin-password"

	if err := svc.EnsureAdmin("admin", password); err != nil {
		t.Fatal(err)
	}
	u, err := svc.Store.UserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != "admin" || u.ID == "" {
		t.Fatalf("unexpected admin record: %#v", u)
	}

	// 已有账号不会被 .env 的新值悄悄改掉。
	if err := svc.EnsureAdmin("admin", "another-password-entirely"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Login("admin", password); err != nil {
		t.Fatalf("original administrator password stopped working: %v", err)
	}
}

func TestEnsureAdminRejectsWeakPassword(t *testing.T) {
	svc := newTestService(t)
	err := svc.EnsureAdmin("admin", "short")
	if err == nil {
		t.Fatal("expected weak administrator password to be rejected")
	}
	var inputErr *InputError
	if !errors.As(err, &inputErr) {
		t.Fatalf("expected *InputError, got %T: %v", err, err)
	}
}

func TestEnsureAdminDoesNotSwallowDatabaseErrors(t *testing.T) {
	svc := newTestService(t)
	// 关闭数据库后 UserByName 会返回真实错误，不应被当成「账号不存在」。
	if err := svc.Store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	err := svc.EnsureAdmin("admin", "a-very-strong-admin-password")
	if err == nil {
		t.Fatal("expected database error to be propagated")
	}
	if errors.Is(err, sqlite.ErrNotFound) {
		t.Fatal("database failure must not be reported as ErrNotFound")
	}
}

func TestEnsureAdminRejectsNonAdminUsernameCollision(t *testing.T) {
	svc := newTestService(t)
	// 普通用户先占用了 ADMIN_USERNAME；EnsureAdmin 不能静默跳过，
	// 否则系统会在没有任何管理员的情况下正常启动。
	if _, err := svc.CreateUser("admin", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureAdmin("admin", "a-very-strong-admin-password"); err == nil {
		t.Fatal("expected a collision error when ADMIN_USERNAME is taken by a regular user")
	}
}

func TestLoginTimingGuardIsStable(t *testing.T) {
	guard := dummyPasswordHash()
	if len(guard) == 0 {
		t.Fatal("timing guard hash is empty")
	}
	if string(guard) != string(dummyPasswordHash()) {
		t.Fatal("timing guard hash is not stable across calls")
	}
	svc := newTestService(t)
	if _, _, err := svc.Login("missing-user", "whatever-password-12"); err == nil {
		t.Fatal("login for a missing user must fail")
	}
}

func TestCreateUserReturnsUsableID(t *testing.T) {
	svc := newTestService(t)
	id, err := svc.CreateUser("alice", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || id == "created" {
		t.Fatalf("CreateUser returned an unusable userId: %q", id)
	}
	u, err := svc.Store.UserByID(id)
	if err != nil {
		t.Fatalf("returned userId cannot be looked up: %v", err)
	}
	if u.Username != "alice" || u.Role != "user" {
		t.Fatalf("unexpected user record: %#v", u)
	}
}

func TestCreateUserValidatesInput(t *testing.T) {
	svc := newTestService(t)
	for name, args := range map[string][2]string{
		"empty username": {"", "correct-horse-battery"},
		"short password": {"alice", "short"},
		"long password":  {"alice", string(make([]byte, 73))},
	} {
		if _, err := svc.CreateUser(args[0], args[1]); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	if _, err := svc.CreateUser("alice", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateUser("alice", "correct-horse-battery"); err == nil {
		t.Fatal("duplicate username must be rejected")
	}
}

func TestFullGroupAuthorizationChain(t *testing.T) {
	svc := newTestService(t)
	store := svc.Store

	userID, err := svc.CreateUser("alice", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	const userGroup, deviceGroup = "ug-1", "dg-1"
	if err := store.CreateGroup(userGroup, "运营组"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateGroup(deviceGroup, "设备组"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUserGroup(userID, userGroup); err != nil {
		t.Fatalf("assign the newly created user to a group: %v", err)
	}
	deviceID := "device-a-0001"
	if err := svc.RegisterDevice(deviceID, "Model-X"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddDeviceGroup(deviceID, deviceGroup); err != nil {
		t.Fatal(err)
	}

	allowed, err := store.CanAccess(userID, deviceID, false)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("device must stay invisible until group_access is configured")
	}

	if err := store.SetGroupAccess(userGroup, deviceGroup, true, false); err != nil {
		t.Fatal(err)
	}
	allowed, err = store.CanAccess(userID, deviceID, false)
	if err != nil || !allowed {
		t.Fatalf("can_view did not grant visibility: allowed=%v err=%v", allowed, err)
	}
	manage, err := store.CanAccess(userID, deviceID, true)
	if err != nil {
		t.Fatal(err)
	}
	if manage {
		t.Fatal("can_view must not imply can_manage")
	}

	// canManage=true 由 SetGroupAccess 强制补上 canView。
	if err := store.SetGroupAccess(userGroup, deviceGroup, false, true); err != nil {
		t.Fatal(err)
	}
	manage, err = store.CanAccess(userID, deviceID, true)
	if err != nil || !manage {
		t.Fatalf("can_manage not granted: allowed=%v err=%v", manage, err)
	}
	allowed, err = store.CanAccess(userID, deviceID, false)
	if err != nil || !allowed {
		t.Fatalf("can_manage should imply can_view: allowed=%v err=%v", allowed, err)
	}
}

func TestLoginAndAuthenticate(t *testing.T) {
	svc := newTestService(t)
	if err := svc.EnsureAdmin("admin", "a-very-strong-admin-password"); err != nil {
		t.Fatal(err)
	}
	token, u, err := svc.Login("admin", "a-very-strong-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("empty session token")
	}
	got, err := svc.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID {
		t.Fatalf("authenticated user = %q, want %q", got.ID, u.ID)
	}
	if _, _, err := svc.Login("admin", "definitely-wrong-password"); err == nil {
		t.Fatal("wrong password must be rejected")
	}
	if _, err := svc.Authenticate("not-a-real-token"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestTicketIsSingleUse(t *testing.T) {
	svc := newTestService(t)
	raw, err := svc.CreateTicket("admin", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := svc.Store.ConsumeTicket(sqlite.Hash(raw))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Type != "admin" || ticket.SubjectID != "user-1" {
		t.Fatalf("unexpected ticket: %#v", ticket)
	}
	if _, err := svc.Store.ConsumeTicket(sqlite.Hash(raw)); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("ticket must not be reusable, got %v", err)
	}
}
