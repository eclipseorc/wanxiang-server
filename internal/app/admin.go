package app

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/envfile"
	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/eclipseorc/wanxiang-server/internal/transport/adminapi"
)

// RunAdmin 初始化 SQLite 和管理员账户，启动管理 API 与静态网页服务（端口 9002）。
func RunAdmin() error {
	if err := envfile.Load(".env"); err != nil {
		return err
	}

	dbPath := envfile.String("DB_PATH", "./data/wanxiang.db")
	log.Printf("opening SQLite database: %s", dbPath)
	// OpenReady 同时完成连接与建表，并对两进程同时启动时的锁竞争做重试。
	db, err := sqlite.OpenReady(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	store := sqlite.NewStore(db)
	// 启动时清理一次遗留的过期凭据，避免历史数据长期堆积。
	if err := store.PurgeExpired(); err != nil {
		return err
	}

	service := logic.New(store)
	if err := service.EnsureAdmin(
		envfile.String("ADMIN_USERNAME", "admin"),
		envfile.String("ADMIN_PASSWORD", ""),
	); err != nil {
		return err
	}
	log.Printf("administrator account is ready")

	certFile := envfile.String("TLS_CERT", "")
	keyFile := envfile.String("TLS_KEY", "")
	if (certFile == "") != (keyFile == "") {
		return errors.New("TLS_CERT and TLS_KEY must be configured together")
	}

	webDir := envfile.String("WEB_DIR", "./web")
	mux := http.NewServeMux()

	// 远程控制需要 adb 能力；定位失败时不阻塞管理后台启动，
	// 只在有配对/连接请求时返回「scrcpy not configured」。
	router := adminapi.New(service)
	if adb, err := scrcpy.NewADB("."); err == nil {
		router.WithScrcpy(adb)
	} else {
		log.Printf("warning: scrcpy disabled: %v", err)
	}

	mux.Handle("/api/", router.Handler())
	mux.Handle("/", dashboardHandler(webDir))

	server := &http.Server{
		Addr:              envfile.String("ADMIN_PORT", ":9002"),
		Handler:           logRequests("admin-api", adminapi.SecurityHeaders(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if certFile != "" {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("admin API and management UI listening on %s (tls=%t)", server.Addr, certFile != "")
		if certFile != "" {
			serverErrors <- server.ListenAndServeTLS(certFile, keyFile)
			return
		}
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

// allowedStaticExt 限制静态服务可返回的文件类型。即使 WEB_DIR 被误配到
// 仓库根目录，也不会把 .env、go.mod、数据库等文件暴露出去。
var allowedStaticExt = map[string]bool{
	".html": true, ".js": true, ".css": true, ".ico": true, ".svg": true,
	".png": true, ".jpg": true, ".woff2": true, ".map": true,
}

// justFiles 包装静态文件系统，禁止 http.FileServer 自动生成目录列表。
// 管理后台只应暴露白名单内的文件；列出目录会泄露部署结构。
type justFiles struct{ fs http.FileSystem }

func (j justFiles) Open(name string) (http.File, error) {
	if !allowedStaticExt[strings.ToLower(filepath.Ext(name))] {
		return nil, os.ErrNotExist
	}
	file, err := j.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.IsDir() {
		_ = file.Close()
		return nil, os.ErrNotExist
	}
	return file, nil
}

// dashboardHandler injects the transport framework before the inline dashboard
// script creates a socket, then loads the bridge and feature modules after the
// page's existing functions have initialized.
func dashboardHandler(webDir string) http.Handler {
	files := http.FileServer(justFiles{fs: http.Dir(webDir)})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.Method != http.MethodGet {
			files.ServeHTTP(w, r)
			return
		}

		page, err := os.ReadFile(filepath.Join(webDir, "index.html"))
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
		const modules = `<script src="/admin-dashboard-bridge.js"></script><script src="/modules/device-events.js"></script><script src="/admin-heartbeat.js"></script><script src="/scrcpy-control.js"></script></body>`
		content = strings.Replace(content, closingBody, modules, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
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
