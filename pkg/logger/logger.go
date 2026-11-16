package logger

import (
	"log"
	"os"
)

// Logger 是标准库 log.Logger 的轻量封装，预留给后续统一日志格式与输出目标。
// 当前两个服务进程仍直接使用标准库 log，本包暂未被引用。
type Logger struct{ *log.Logger }

// New 创建一个带 UTC 微秒时间戳、输出到标准输出的记录器。
func New() *Logger {
	return &Logger{Logger: log.New(os.Stdout, "", log.LstdFlags|log.LUTC|log.Lmicroseconds)}
}
