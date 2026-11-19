package app

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
	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
)

// RunScrcpy 提供远程屏幕查看与控制服务（端口 9004），是 scrcpy 的 WebSocket 代理。
//
// 职责边界：
//   - 通过 adb（tools/ 下随项目分发）部署、启动设备上的 scrcpy-server。
//   - 在浏览器与设备之间桥接 H.264 视频流与控制事件（一次 WS 连接 = 一次观看）。
//
// 设备配对与连接（adb pair/connect）由 admin-server 负责，本服务只处理观看会话，
// 不依赖数据库。
func RunScrcpy() error {
	if err := envfile.Load(".env"); err != nil {
		return err
	}

	// 定位并自检 adb，找不到时立即失败，避免运行到一半才发现无法控制设备。
	adb, err := scrcpy.NewADB(".")
	if err != nil {
		return err
	}
	version, err := adb.Version(context.Background())
	if err != nil {
		return err
	}
	log.Printf("adb ready: %s", version)

	scrcpySrv := scrcpy.NewServer(adb, scrcpy.DefaultScrcpyConfig("."))

	server := &http.Server{
		Addr:              envfile.String("SCRCPY_PORT", ":9004"),
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           scrcpySrv.ServeMux(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("scrcpy relay listening on %s", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			return err
		}
	case <-ctx.Done():
		log.Printf("shutting down scrcpy relay")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
