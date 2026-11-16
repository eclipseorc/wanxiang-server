// Command web-server 启动面向安卓设备端的 HTTP API。
// 设备不登录：以 deviceId 作为凭证换取一次性 WebSocket 连接票据。
// 本服务不提供静态页面，也不涉及管理员账户初始化。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/envfile"
	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/eclipseorc/wanxiang-server/internal/transport/webapi"
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
	// OpenReady 同时完成连接与建表，并对多进程同时启动时的锁竞争做重试。
	db, err := sqlite.OpenReady(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	store := sqlite.NewStore(db)
	service := logic.New(store)

	mux := http.NewServeMux()
	mux.Handle("/api/", webapi.New(service).Handler())

	server := &http.Server{
		Addr:              envfile.String("WEB_ADDR", ":9001"),
		Handler:           logRequests("web-api", mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("web API listening on %s", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("shutting down web server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

// logRequests 记录请求元数据，刻意排除可能含一次性票据的 query string。
func logRequests(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		log.Printf("%s request started method=%s path=%s remote=%s", service, r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		log.Printf("%s request completed method=%s path=%s duration=%s", service, r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}
