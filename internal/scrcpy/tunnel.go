// tunnel.go 负责设备侧 scrcpy-server 的部署、启动，以及 adb forward 与
// 视频/控制两条 TCP 连接的建立。
//
// 一次观看建立两条连接（scrcpy --tunnel-forward 模式约定）：
//   - 视频连接：首次 accept，返回 codec 头 + H.264 帧流
//   - 控制连接：第二次 accept，用于写入控制事件
//
// 两条连接都通过同一个 localabstract:scrcpy socket 转发到本机同一端口，
// 客户端需要连接两次。
package scrcpy

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ScrcpyConfig 是 scrcpy-server 的启动参数，与 server.js 的默认值保持一致。
type ScrcpyConfig struct {
	ServerLocalPath  string // 本地 scrcpy-server jar 路径
	ServerRemotePath string // 设备上的部署路径
	MaxSize          int    // 视频最大边长
	BitRate          int    // 视频码率 bps
	MaxFPS           int    // 最大帧率
}

// DefaultScrcpyConfig 返回基于 baseDir（仓库根）的默认配置。
// 局域网带宽充足，MaxSize 提到 1920、码率 8Mbps，兼顾清晰度与流畅度。
func DefaultScrcpyConfig(baseDir string) ScrcpyConfig {
	return ScrcpyConfig{
		ServerLocalPath:  filepath.Join(baseDir, "tools", "scrcpy-server"),
		ServerRemotePath: "/data/local/tmp/scrcpy-server",
		MaxSize:          1920,
		BitRate:          8000000,
		MaxFPS:           60,
	}
}

// buildScrcpyCommand 拼出 adb shell 中启动 scrcpy-server 的完整命令。
// tunnel_forward=true 让 scrcpy-server 在设备上创建 localabstract:scrcpy 等待连接。
func (c ScrcpyConfig) buildScrcpyCommand() string {
	args := []string{
		fmt.Sprintf("CLASSPATH=%s", c.ServerRemotePath),
		"app_process",
		"/",
		"com.genymobile.scrcpy.Server",
		"4.0",
		"audio=false",
		"tunnel_forward=true",
		fmt.Sprintf("max_size=%d", c.MaxSize),
		fmt.Sprintf("video_bit_rate=%d", c.BitRate),
		fmt.Sprintf("max_fps=%d", c.MaxFPS),
		"video_codec=h264",
		"control=true",
		"send_device_meta=false",
		"send_frame_meta=true",
		"send_dummy_byte=false",
	}
	return strings.Join(args, " ")
}

// deploy 把 scrcpy-server 部署到设备。若设备上文件大小一致则跳过 push。
func (c ScrcpyConfig) deploy(ctx context.Context, adb *ADB, serial string) error {
	localSize, err := fileSize(c.ServerLocalPath)
	if err != nil {
		return fmt.Errorf("read local scrcpy-server: %w", err)
	}

	remoteSize, err := adb.remoteFileSize(ctx, serial, c.ServerRemotePath)
	if err == nil && remoteSize == localSize {
		return nil // 已在设备上，跳过 push
	}

	if err := adb.Push(ctx, serial, c.ServerLocalPath, c.ServerRemotePath); err != nil {
		return fmt.Errorf("push scrcpy-server: %w", err)
	}

	// push 后做二次验证，确认大小一致。
	verified, err := adb.remoteFileSize(ctx, serial, c.ServerRemotePath)
	if err != nil {
		return fmt.Errorf("verify pushed scrcpy-server: %w", err)
	}
	if verified != localSize {
		return fmt.Errorf("push verification failed: local=%d remote=%d", localSize, verified)
	}

	// 确保可执行。
	if _, err := adb.Shell(ctx, serial, "chmod 755 "+c.ServerRemotePath); err != nil {
		return fmt.Errorf("chmod scrcpy-server: %w", err)
	}
	return nil
}

// start 在设备上启动 scrcpy-server（后台 shell 命令），并等待其 socket 就绪。
func (c ScrcpyConfig) start(ctx context.Context, adb *ADB, serial string) error {
	command := c.buildScrcpyCommand()
	// 使用 shell 后台执行（&），因为 adb shell 是同步阻塞的，scrcpy-server 常驻。
	if _, err := adb.Shell(ctx, serial, command+" >/dev/null 2>&1 &"); err != nil {
		return fmt.Errorf("start scrcpy-server: %w", err)
	}

	// 等待 localabstract:scrcpy socket 出现。
	if err := waitScrcpySocket(ctx, adb, serial, true, 10*time.Second); err != nil {
		return err
	}
	return nil
}

// waitScrcpySocket 轮询 /proc/net/unix 直到 scrcpy socket 出现（exists=true）或消失。
func waitScrcpySocket(ctx context.Context, adb *ADB, serial string, exists bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		found, err := scrcpySocketExists(ctx, adb, serial)
		if err == nil && found == exists {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if exists {
		return fmt.Errorf("scrcpy-server did not create localabstract:scrcpy within %s", timeout)
	}
	return fmt.Errorf("old scrcpy-server still holds localabstract:scrcpy after %s", timeout)
}

// scrcpySocketExists 检查设备上是否存在 scrcpy 的 localabstract socket。
func scrcpySocketExists(ctx context.Context, adb *ADB, serial string) (bool, error) {
	out, err := adb.Shell(ctx, serial, "cat /proc/net/unix")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "scrcpy"), nil
}

// tunnel 表示一次观看建立的两条 TCP 连接（视频 + 控制）。
type tunnel struct {
	video   net.Conn
	control net.Conn
}

// establish 在设备已部署并 forward 之后，连接视频与控制两条通道。
// 按 scrcpy 约定，第一次连接返回视频通道，第二次返回控制通道。
func establish(ctx context.Context, localPort int) (*tunnel, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))

	video, err := dial(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("connect video channel: %w", err)
	}
	control, err := dial(ctx, addr)
	if err != nil {
		video.Close()
		return nil, fmt.Errorf("connect control channel: %w", err)
	}
	return &tunnel{video: video, control: control}, nil
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// close 关闭两条连接。
func (t *tunnel) close() {
	if t.video != nil {
		_ = t.video.Close()
	}
	if t.control != nil {
		_ = t.control.Close()
	}
}

// fileSize 返回本地文件大小。
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// remoteFileSize 返回设备上指定路径的文件大小；不存在时返回 -1。
func (a *ADB) remoteFileSize(ctx context.Context, serial, path string) (int64, error) {
	out, err := a.Shell(ctx, serial, "stat -c %s "+path)
	if err != nil {
		return -1, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return -1, fmt.Errorf("file %s not found", path)
	}
	return strconv.ParseInt(out, 10, 64)
}
