// Command http-server runs the management API and serves the static console.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/envfile"
	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/eclipseorc/wanxiang-server/internal/transport/httpapi"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if err := envfile.Load(".env"); err != nil {
		return err
	}

	dbPath := envfile.String("DB_PATH", "./data/wanxiang.db")
	log.Printf("opening SQLite database: %s", dbPath)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := sqlite.InitSchema(db); err != nil {
		return err
	}

	service := logic.New(sqlite.NewStore(db))
	if err := service.EnsureAdmin(
		envfile.String("ADMIN_USERNAME", "admin"),
		envfile.String("ADMIN_PASSWORD", ""),
	); err != nil {
		return err
	}
	log.Printf("administrator account is ready")

	webDir := envfile.String("WEB_DIR", "./web")
	mux := http.NewServeMux()
	mux.Handle("/api/", httpapi.New(service).Handler())
	mux.Handle("/", dashboardHandler(webDir))

	server := &http.Server{
		Addr:              envfile.String("HTTP_ADDR", ":8080"),
		Handler:           logRequests("http-api", mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("HTTP API and management UI listening on %s", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("shutting down HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

// dashboardHandler injects the transport framework before the inline dashboard
// script creates a socket, then loads the heartbeat plugin after the dashboard's
// own ws variable and connection functions have been initialized.
func dashboardHandler(webDir string) http.Handler {
	files := http.FileServer(http.Dir(webDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.Method != http.MethodGet {
			files.ServeHTTP(w, r)
			return
		}

		page, err := os.ReadFile(webDir + string(os.PathSeparator) + "index.html")
		if err != nil {
			log.Printf("read dashboard index: %v", err)
			http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
			return
		}

		content := string(page)
		const closingHead = "</head>"
		const closingBody = "</body>"
		if !strings.Contains(content, closingHead) || !strings.Contains(content, closingBody) {
			http.Error(w, "dashboard index is missing closing head/body tag", http.StatusInternalServerError)
			return
		}
		content = strings.Replace(content, closingHead, `<script src="/ws-framework.js"></script></head>`, 1)
		content = strings.Replace(content, closingBody, `<script src="/admin-heartbeat.js"></script></body>`, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(content))
	})
}

// logRequests logs request metadata but deliberately excludes query strings,
// which may contain short-lived WebSocket tickets.
func logRequests(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		log.Printf("%s request started method=%s path=%s remote=%s", service, r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		log.Printf("%s request completed method=%s path=%s duration=%s", service, r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}
