// Package app 提供多服务的单进程编排能力。
//
// 万象后端由四个可独立运行的 HTTP 服务组成：web(9001)、admin(9002)、
// websocket(9003)、scrcpy(9004)。它们既保留各自的 cmd 入口以便单独编译
// 部署，也通过 cmd/wanxiang 这一总入口在单个进程内聚合启动。
//
// Supervise 是聚合启动的核心：为每个服务起一个 goroutine，用 recover 隔离
// panic，使任一服务崩溃只影响它自己，其余服务继续运行；同时支持失败后按
// 指数退避自动重启，从而在单进程形态下仍保持「一个服务挂了不拖累其他」的
// 隔离体验。
package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Service 描述一个可被 supervisor 编排的服务。
//
// Name 用于日志与故障标识；Run 是服务的启动入口，返回 nil 表示正常退出，
// 返回非 nil 错误表示服务失败（supervisor 将据此决定是否重启）。
type Service struct {
	Name string
	Run  func() error
}

// Supervise 启动全部服务并监管其生命周期，直到收到退出信号或全部服务
// 停止运行。单个服务的 panic 会被捕获并转化为错误，不会导致进程崩溃。
//
// 行为约定：
//   - 每个服务在自己的 goroutine 中运行，panic 被 recover 隔离；
//   - 服务失败后按指数退避自动重启，退避上限约 30 秒；
//   - 收到 SIGINT/SIGTERM 时发起优雅退出，等待各 goroutine 自行结束；
//   - 当所有服务都退出（例如全部返回 nil）时，Supervise 返回。
func Supervise(services []Service) {
	if len(services) == 0 {
		log.Printf("no services to supervise")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for _, svc := range services {
		wg.Add(1)
		go superviseOne(ctx, svc, &wg)
	}

	// 主 goroutine 只负责等待退出信号或全部服务结束。
	<-ctx.Done()
	log.Printf("shutdown signal received; waiting for services to stop")
	wg.Wait()
	log.Printf("all services stopped")
}

// superviseOne 在独立 goroutine 中运行单个服务，提供 panic 隔离与失败重启。
func superviseOne(ctx context.Context, svc Service, wg *sync.WaitGroup) {
	defer wg.Done()

	const (
		baseBackoff  = 500 * time.Millisecond
		maxBackoff   = 30 * time.Second
		backoffReset = 30 * time.Second // 稳定运行超过该时长后清零退避计数
	)

	var (
		backoff  = baseBackoff
		restarts int
		started  = time.Now()
	)

	for {
		// 用独立函数封装 Run，便于 recover 捕获 panic 并转化为错误。
		err := runGuarded(svc.Name, svc.Run)

		// 收到退出信号后返回的 nil 视为正常退出，不再重启。
		if err == nil || ctx.Err() != nil {
			log.Printf("[%s] stopped", svc.Name)
			return
		}

		// 服务已稳定运行足够久后又失败，说明退避周期早已结束，重置退避，
		// 让重启保持轻量（快速拉起），而不是沿用上一次的长时间等待。
		if time.Since(started) >= backoffReset {
			backoff = baseBackoff
			restarts = 0
		}
		restarts++
		log.Printf("[%s] failed: %v (restart #%d, backoff %s)",
			svc.Name, err, restarts, backoff)

		// 指数退避等待后重启；等待期间若收到退出信号则停止。
		select {
		case <-ctx.Done():
			log.Printf("[%s] aborting restart due to shutdown", svc.Name)
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runGuarded 调用服务的 Run，将 panic 捕获后包装为错误返回，保证单个
// 服务的崩溃不会向上传播到整个进程。
func runGuarded(name string, run func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s panicked: %v", name, r)
			log.Printf("[%s] recovered from panic: %v", name, r)
		}
	}()
	return run()
}
