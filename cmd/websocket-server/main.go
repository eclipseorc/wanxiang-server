// Command websocket-server 初始化共享数据库和 WebSocket Hub，启动设备/管理端转发服务。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/envfile"
	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	wsserver "github.com/eclipseorc/wanxiang-server/internal/transport/websocket"
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
	// 浏览器管理端总会带 Origin；白名单为空时这些连接会被全部拒绝。
	// 启动时明确告警，避免只表现为前端「连接失败」而难以定位。
	if strings.TrimSpace(os.Getenv("WS_ALLOWED_ORIGINS")) == "" {
		log.Printf("warning: WS_ALLOWED_ORIGINS is empty; browser admin WebSocket connections will be rejected")
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

	certFile := envfile.String("TLS_CERT", "")
	keyFile := envfile.String("TLS_KEY", "")
	if (certFile == "") != (keyFile == "") {
		return errors.New("TLS_CERT and TLS_KEY must be configured together")
	}

	hub := wsserver.NewHub(store)

	// 装配 adb 状态扫描器：周期检测「业务在线但 adb 离线」的设备并下发 adb-status。
	// adb 找不到时扫描器会静默停用（远程控制本就不可用），不影响转发服务。
	if adb, err := scrcpy.NewADB("."); err == nil {
		monitor := wsserver.NewADBMonitor(hub, adb)
		monitor.Start(context.Background())
	} else {
		log.Printf("adb-status monitor disabled: %v", err)
	}

	server := &http.Server{
		Addr:              envfile.String("WS_PORT", ":9003"),
		Handler:           logRequests("websocket", wsserver.NewHandler(hub).ServeMux()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if certFile != "" {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("WebSocket relay listening on %s (tls=%t)", server.Addr, certFile != "")
		if certFile != "" {
			serverErrors <- server.ListenAndServeTLS(certFile, keyFile)
			return
		}
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			return err
		}
	case <-ctx.Done():
		log.Printf("shutting down WebSocket relay")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return nil
}

// logRequests reports WebSocket upgrade attempts without logging query strings,
// which can contain one-use authentication tickets. The start log appears as soon
// as the client connects; the completion log appears when that socket closes.
func logRequests(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		log.Printf("%s request started method=%s path=%s remote=%s", service, r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		log.Printf("%s request completed method=%s path=%s duration=%s", service, r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}
