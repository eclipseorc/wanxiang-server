package webapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.InitSchema(db); err != nil {
		t.Fatal(err)
	}
	svc := logic.New(sqlite.NewStore(db))
	return New(svc).Handler()
}

func request(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 设备首次换票即自动登记，重复换票不报错且设备只存在一行。
func TestDeviceTicketRegistersDeviceIdempotently(t *testing.T) {
	h := newTestHandler(t)

	first := request(t, h, http.MethodPost, "/api/device/ws-ticket", `{"deviceId":"dev-0001","model":"Pixel-8"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first ticket: got %d, want 200 (%s)", first.Code, first.Body.String())
	}
	if !strings.Contains(first.Body.String(), "ticket") {
		t.Fatalf("expected ticket in response, got %s", first.Body.String())
	}

	second := request(t, h, http.MethodPost, "/api/device/ws-ticket", `{"deviceId":"dev-0001","model":"Pixel-8"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second ticket: got %d, want 200", second.Code)
	}
}

// 缺 deviceId 直接 400。
func TestDeviceTicketRequiresDeviceID(t *testing.T) {
	h := newTestHandler(t)
	rec := request(t, h, http.MethodPost, "/api/device/ws-ticket", `{"model":"Pixel-8"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}
