// adb.go 封装对 Android Debug Bridge 的调用，是“服务器统一跑 adb”模式的执行层。
//
// 设计约定：
//   - adb 二进制随项目分发在 tools/ 目录（Windows 为 tools/adb.exe，其他为 tools/adb），
//     通过 resolveAdbPath 解析，避免依赖系统 PATH 或额外安装。
//   - 所有命令都通过 exec.Command 执行，带超时；输出以 error 返回而非吞掉。
//   - 方法按“一次命令”的粒度组织，会话层负责组合成完整流程。
package scrcpy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultAdbTimeout 是单条 adb 命令的默认超时。adb pair / connect 在弱网下可能较慢。
const defaultAdbTimeout = 15 * time.Second

// ADB 封装了 adb 二进制的路径与命令执行。
type ADB struct {
	path string
}

// NewADB 定位项目内的 adb 二进制并返回封装。找不到时返回错误。
// baseDir 通常是仓库根目录，adb 位于 <baseDir>/tools/。
func NewADB(baseDir string) (*ADB, error) {
	path, err := resolveAdbPath(baseDir)
	if err != nil {
		return nil, err
	}
	return &ADB{path: path}, nil
}

// resolveAdbPath 在 baseDir 下查找 tools/adb（Windows 带 .exe 后缀）。
func resolveAdbPath(baseDir string) (string, error) {
	name := "adb"
	if runtime.GOOS == "windows" {
		name = "adb.exe"
	}
	candidate := filepath.Join(baseDir, "tools", name)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate, nil
	}
	// 兜底：尝试不带后缀（兼容符号链接或非标准命名）。
	alt := filepath.Join(baseDir, "tools", "adb")
	if info, err := os.Stat(alt); err == nil && !info.IsDir() {
		return alt, nil
	}
	return "", fmt.Errorf("adb not found under %s", filepath.Join(baseDir, "tools"))
}

// run 执行一条 adb 命令并返回标准输出。args 不含 adb 本身。
func (a *ADB) run(ctx context.Context, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, defaultAdbTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, a.path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("adb %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// Version 返回 adb 版本号，用于启动自检。
func (a *ADB) Version(ctx context.Context) (string, error) {
	out, err := a.run(ctx, "version")
	if err != nil {
		return "", err
	}
	// 输出形如 "Android Debug Bridge version 1.0.41"，取第一行。
	line := strings.SplitN(out, "\n", 2)[0]
	return strings.TrimSpace(line), nil
}

// Devices 返回当前 adb 可见的设备列表，按行给出 serial 与状态。
// serial 是后续所有设备级命令（-s serial）的目标标识。
func (a *ADB) Devices(ctx context.Context) ([]string, error) {
	out, err := a.run(ctx, "devices")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// EnsureServer 启动 adb server（若未运行），保证后续命令有稳定的 daemon。
// 无线调试场景下 daemon 意外退出会导致设备连接丢失，因此在长流程开始前调用。
func (a *ADB) EnsureServer(ctx context.Context) error {
	_, err := a.run(ctx, "start-server")
	return err
}

// IsDeviceOnline 判断指定 serial 是否处于 device（在线可操作）状态。
func (a *ADB) IsDeviceOnline(ctx context.Context, serial string) (bool, error) {
	out, err := a.run(ctx, "devices")
	if err != nil {
		return false, err
	}
	for _, s := range parseOnlineSerials(out) {
		if s == serial {
			return true, nil
		}
	}
	return false, nil
}

// OnlineDevices 返回 adb 中处于 device（在线可操作）状态的设备 serial 列表。
// 这些设备已完成配对与连接，可立即用于观看/群控。serial 即无线设备的 ip:port。
func (a *ADB) OnlineDevices(ctx context.Context) ([]string, error) {
	out, err := a.run(ctx, "devices")
	if err != nil {
		return nil, err
	}
	return parseOnlineSerials(out), nil
}

// parseOnlineSerials 从 `adb devices` 的输出中提取处于 device 状态的 serial。
// 输出形如：
//
//	List of devices attached
//	192.168.1.100:35583\tdevice
//	192.168.1.101:40000\toffline
//
// 只返回状态为 device 的行（已完成配对+连接，可操作）。
func parseOnlineSerials(out string) []string {
	var serials []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "device" {
			serials = append(serials, fields[0])
		}
	}
	return serials
}

// Pair 执行无线调试配对。pairPort 是设备「无线调试」页显示的配对端口，
// code 是 6 位配对码。设备已配对过时返回 nil（幂等），避免重复配对报错。
func (a *ADB) Pair(ctx context.Context, host string, pairPort int, code string) error {
	_, err := a.run(ctx, "pair", fmt.Sprintf("%s:%d", host, pairPort), code)
	if err == nil {
		return nil
	}
	// 已配对/已连接时 adb 会报错，属正常幂等场景，不视为失败。
	if strings.Contains(err.Error(), "already paired") ||
		strings.Contains(err.Error(), "already connected") ||
		strings.Contains(err.Error(), "protocol fault") {
		return nil
	}
	return err
}

// Connect 建立无线调试连接。port 是设备「无线调试」页显示的连接端口（非配对端口）。
func (a *ADB) Connect(ctx context.Context, host string, port int) error {
	_, err := a.run(ctx, "connect", fmt.Sprintf("%s:%d", host, port))
	return err
}

// Reconnect 对掉线的无线设备重连。serial 形如 host:port。
func (a *ADB) Reconnect(ctx context.Context, serial string) error {
	host, port, err := splitHostPort(serial)
	if err != nil {
		return fmt.Errorf("reconnect %s: %w", serial, err)
	}
	return a.Connect(ctx, host, port)
}

// splitHostPort 把 ip:port 形式的 serial 拆开。adb 无线设备的 serial 即 ip:port。
func splitHostPort(serial string) (string, int, error) {
	host, portStr, ok := strings.Cut(serial, ":")
	if !ok {
		return "", 0, fmt.Errorf("invalid serial %q, want host:port", serial)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	return host, port, nil
}

// Disconnect 断开与指定 serial 的连接。
func (a *ADB) Disconnect(ctx context.Context, serial string) error {
	_, err := a.run(ctx, "disconnect", serial)
	return err
}

// Forward 把设备的 localabstract socket 转发到本机 port。
// 用于把设备上的 scrcpy-server 监听转发给本进程连接。
func (a *ADB) Forward(ctx context.Context, serial string, localPort int, remote string) error {
	_, err := a.run(ctx, "-s", serial, "forward", fmt.Sprintf("tcp:%d", localPort), remote)
	return err
}

// RemoveForward 移除指定的本地端口转发。
func (a *ADB) RemoveForward(ctx context.Context, serial string, localPort int) error {
	_, err := a.run(ctx, "-s", serial, "forward", "--remove", fmt.Sprintf("tcp:%d", localPort))
	return err
}

// Push 把本地文件推送到设备指定路径。
func (a *ADB) Push(ctx context.Context, serial, localPath, remotePath string) error {
	_, err := a.run(ctx, "-s", serial, "push", localPath, remotePath)
	return err
}

// Shell 在设备上执行 shell 命令并返回输出。
func (a *ADB) Shell(ctx context.Context, serial string, command string) (string, error) {
	return a.run(ctx, "-s", serial, "shell", command)
}

// DeviceProp 读取设备的系统属性，如 ro.product.model、ro.build.version.release。
func (a *ADB) DeviceProp(ctx context.Context, serial, prop string) (string, error) {
	out, err := a.Shell(ctx, serial, "getprop "+prop)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ListForwards 返回设备当前的端口转发列表。
func (a *ADB) ListForwards(ctx context.Context, serial string) (string, error) {
	return a.run(ctx, "-s", serial, "forward", "--list")
}
