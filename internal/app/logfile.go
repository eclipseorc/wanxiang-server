package app

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SetupLogging 配置标准库 log 同时输出到控制台（stderr）与按天切分的日志文件
// （logs/<name>-YYYY-MM-DD.log），确保用户直接运行 .exe 或 go run 时也能自动落盘日志，
// 且文件名不与 scripts/run.ps1 的重定向日志冲突。
func SetupLogging(name string) func() {
	if name == "" {
		name = "wanxiang"
	}
	w, err := newDailyLogWriter("logs", name)
	if err != nil {
		log.Printf("warning: init file logger failed (falling back to stderr only): %v", err)
		return func() {}
	}
	log.SetOutput(io.MultiWriter(os.Stderr, w))
	return func() {
		_ = w.Close()
	}
}

type dailyLogWriter struct {
	dir  string
	name string

	mu   sync.Mutex
	day  string
	file *os.File
}

func newDailyLogWriter(dir, name string) (*dailyLogWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &dailyLogWriter{dir: dir, name: name}
	if err := w.rotateLocked(time.Now().Format("2006-01-02")); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if today != w.day || w.file == nil {
		if err := w.rotateLocked(today); err != nil {
			return 0, err
		}
	}
	return w.file.Write(p)
}

func (w *dailyLogWriter) rotateLocked(day string) error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	path := filepath.Join(w.dir, fmt.Sprintf("%s-%s.log", w.name, day))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.day = day
	w.file = f
	return nil
}

func (w *dailyLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
