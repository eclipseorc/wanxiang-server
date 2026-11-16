package logger

import "os"

// EnsureDir 幂等地创建日志目录，权限限制为仅属主可读写、同组可读。
// 供后续把日志落盘时使用；当前服务只输出到控制台，暂未被引用。
func EnsureDir(path string) error { return os.MkdirAll(path, 0o750) }
