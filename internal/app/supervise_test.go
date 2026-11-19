package app

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunGuardedRecoversPanic 验证 panic 被 recover 转化为错误，而非向上传播。
func TestRunGuardedRecoversPanic(t *testing.T) {
	err := runGuarded("s", func() error { panic("boom") })
	if err == nil {
		t.Fatal("expected panic to be recovered as error, got nil")
	}
}

// TestRunGuardedReturnsError 验证正常返回的错误被原样透传。
func TestRunGuardedReturnsError(t *testing.T) {
	sentinel := errors.New("sentinel")
	err := runGuarded("s", func() error { return sentinel })
	if err != sentinel {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// TestSuperviseIsolatesPanic 验证某个服务 panic 不会拖垮其他服务：
// 一个服务持续 panic，另一个服务应能持续运行并被多次调用。
func TestSuperviseIsolatesPanic(t *testing.T) {
	var healthyCalls int32
	var panicCalls int32

	services := []Service{
		{
			Name: "healthy",
			Run: func() error {
				atomic.AddInt32(&healthyCalls, 1)
				// 模拟一个稳定运行的服务，持续到测试结束（永不返回）。
				select {}
			},
		},
		{
			Name: "panicky",
			Run: func() error {
				atomic.AddInt32(&panicCalls, 1)
				panic("simulated crash")
			},
		},
	}

	// 用独立 goroutine 驱动 supervisor，避免阻塞测试主流程。
	done := make(chan struct{})
	go func() {
		defer close(done)
		Supervise(services)
	}()

	// 给 supervisor 足够长的时间，覆盖至少两个退避周期
	// （baseBackoff=500ms + 翻倍后 1s），确保 panicky 服务被重启多次。
	time.Sleep(1800 * time.Millisecond)

	// 关键断言：panicky 服务因 panic 被重启了至少两次（>2 次调用），
	// 这证明 panic 被隔离且触发了自动重启，而不是让整个进程崩溃。
	if got := atomic.LoadInt32(&panicCalls); got < 3 {
		t.Fatalf("expected panicky service to be restarted multiple times, got %d calls", got)
	}

	// healthy 服务应至少成功运行过一次，且未被 panic 拖垮。
	if got := atomic.LoadInt32(&healthyCalls); got != 1 {
		t.Fatalf("expected healthy service to run exactly once (never restarted), got %d calls", got)
	}
}
