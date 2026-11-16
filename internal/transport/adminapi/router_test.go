package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

const testAdminPassword = "a-very-strong-admin-password"

func newTestRouterWithStore(t *testing.T) (http.Handler, *sqlite.Store) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.InitSchema(db); err != nil {
		t.Fatal(err)
	}
	store := sqlite.NewStore(db)
	svc := logic.New(store)
	if err := svc.EnsureAdmin("admin", testAdminPassword); err != nil {
		t.Fatal(err)
	}
	return New(svc).Handler(), store
}

func newTestRouter(t *testing.T) http.Handler {
	h, _ := newTestRouterWithStore(t)
	return h
}

func request(t *testing.T, h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func loginAdmin(t *testing.T, h http.Handler) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":"admin","password":%q}`, testAdminPassword)
	rec := request(t, h, http.MethodPost, "/api/admin/login", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("login returned an empty token")
	}
	return out.Token
}

func TestLoginRateLimit(t *testing.T) {
	h := newTestRouter(t)
	wrong := `{"username":"admin","password":"definitely-wrong"}`
	for i := 0; i < loginMaxFailures; i++ {
		if rec := request(t, h, http.MethodPost, "/api/admin/login", wrong, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, rec.Code)
		}
	}
	// 即使正确密码，锁定期间也必须被拒绝。
	good := fmt.Sprintf(`{"username":"admin","password":%q}`, testAdminPassword)
	rec := request(t, h, http.MethodPost, "/api/admin/login", good, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429 after repeated failures", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 response is missing Retry-After")
	}
}

func TestCreateUserThenAssignGroup(t *testing.T) {
	h := newTestRouter(t)
	token := loginAdmin(t, h)

	rec := request(t, h, http.MethodPost, "/api/admin/users",
		`{"username":"alice","password":"correct-horse-battery"}`, token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	var user struct {
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.UserID == "" || user.UserID == "created" {
		t.Fatalf("create user returned unusable userId %q", user.UserID)
	}

	rec = request(t, h, http.MethodPost, "/api/admin/groups", `{"name":"运营组"}`, token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", rec.Code, rec.Body.String())
	}
	var group struct {
		GroupID string `json:"groupId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"userId":%q,"groupId":%q}`, user.UserID, group.GroupID)
	rec = request(t, h, http.MethodPost, "/api/admin/groups/users", body, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign newly created user to group: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	h := newTestRouter(t)
	token := loginAdmin(t, h)
	// role 不是合法输入字段，必须被拒绝而不是静默忽略。
	rec := request(t, h, http.MethodPost, "/api/admin/users",
		`{"username":"mallory","password":"correct-horse-battery","role":"admin"}`, token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for unknown field", rec.Code)
	}
}

func TestSecurityHeadersAndMethodChecks(t *testing.T) {
	h := newTestRouter(t)
	rec := request(t, h, http.MethodGet, "/api/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health: %d", rec.Code)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "",
	} {
		got := rec.Header().Get(header)
		if want != "" && got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
		if want == "" && got == "" {
			t.Errorf("%s is missing", header)
		}
	}

	token := loginAdmin(t, h)
	rec = request(t, h, http.MethodGet, "/api/admin/users", "", token)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET users: got %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}

	noToken := request(t, h, http.MethodGet, "/api/admin/devices", "", "")
	if noToken.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: got %d, want 401", noToken.Code)
	}
}

func TestTicketEndpointRateLimit(t *testing.T) {
	h := newTestRouter(t)
	token := loginAdmin(t, h)
	for i := 0; i < ticketMaxPerWindow; i++ {
		if rec := request(t, h, http.MethodPost, "/api/admin/ws-ticket", "", token); rec.Code != http.StatusOK {
			t.Fatalf("ticket %d: got %d, want 200", i+1, rec.Code)
		}
	}
	if rec := request(t, h, http.MethodPost, "/api/admin/ws-ticket", "", token); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429 after %d tickets", rec.Code, ticketMaxPerWindow)
	}
}

func TestLoginLimiterRejectsWhenTableIsFull(t *testing.T) {
	limiter := newLoginLimiter()
	now := time.Now()
	for i := 0; i < limiterMaxEntries; i++ {
		if !limiter.recordFailure(fmt.Sprintf("key-%d", i), now) {
			t.Fatalf("entry %d was rejected before the table was full", i)
		}
	}
	if limiter.recordFailure("a-brand-new-key", now) {
		t.Fatal("a new key must be rejected when the limiter table is saturated")
	}
}

func TestAuditRecordsCriticalActions(t *testing.T) {
	h, store := newTestRouterWithStore(t)
	token := loginAdmin(t, h)
	request(t, h, http.MethodPost, "/api/admin/users",
		`{"username":"alice","password":"correct-horse-battery"}`, token)
	request(t, h, http.MethodPost, "/api/admin/logout", "", token)

	var count int
	if err := store.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action IN ('login','create-user','logout')`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 audit rows, got %d", count)
	}
}
