package app

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
	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	wsserver "github.com/eclipseorc/wanxiang-server/internal/transport/websocket"
)

// RunWebsocket 初始化共享数据库和 WebSocket Hub，启动设备/管理端转发服务（端口 9003）。
func RunWebsocket() error {
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

	// 装配 adb 能力：既是「业务在线但 adb 离线」状态扫描器，也是设备「ADB 就绪」
	// 设备「ADB 就绪」事件的 connect 执行器。两者共享同一个 logic.AdbConnector
	// （连接管理唯一入口），复用同一 adb 实例与共享 SQLite。
	// adb 找不到时静默停用（远程控制本就不可用），不影响转发服务。
	if adb, err := scrcpy.NewADB("."); err == nil {
		connector := logic.NewAdbConnector(logic.New(store), adb)

		monitor := wsserver.NewADBMonitor(hub, connector)
		monitor.Start(context.Background())

		// 注入 adb-ready 处理器：复用同一 connector，把设备本地自配对完成后的
		// connect 动作交给 logic 层统一执行。
		hub.WithAdbConnector(connector)
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
