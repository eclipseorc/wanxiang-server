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
	"io"
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

// adbTransferTimeout 是 adb pull/push 等文件传输类命令的超时。文件传输耗时与
// 文件大小线性相关（实测 48MB APK 经 Wi-Fi 拉取约 18 秒），必须比默认超时宽松得多，
// 否则大文件会在传输中途被 context 杀掉，表现为「下载失败」。
const adbTransferTimeout = 10 * time.Minute

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
	return a.runWithTimeout(ctx, defaultAdbTimeout, args...)
}

// runWithTimeout 执行一条 adb 命令，允许调用方指定超时。文件传输（pull/push）
// 等耗时操作应传入更宽松的超时，避免大文件被默认超时中断。
func (a *ADB) runWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
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

// ResolveOnlinePort 返回指定 IP 当前在 adb 中处于 device 状态的连接端口。
//
// 无线调试的连接端口是动态分配的，connect 成功后 adb devices 里会出现
// 形如 "192.168.1.100:35583" 的 serial。本方法按 IP 前缀匹配，提取该 IP
// 真实在线的端口，供落库（拼真实 serial 用）。找不到（设备离线或端口未知）
// 时返回错误，调用方不应将其当作可用的固定端口。
func (a *ADB) ResolveOnlinePort(ctx context.Context, host string) (int, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return 0, fmt.Errorf("resolve port: empty host")
	}
	out, err := a.run(ctx, "devices")
	if err != nil {
		return 0, err
	}
	for _, serial := range parseOnlineSerials(out) {
		ip, portStr, ok := strings.Cut(serial, ":")
		if !ok || ip != host {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("host %s not online in adb devices", host)
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

// UsbSerials 返回 adb 中通过 USB 有线连接的设备序列号列表（非 ip:port 形式）。
// 用于「传统模式」切 tcpip 时，让前端下拉选择设备、免去用户手填序列号。
// 包含 device 与 unauthorized 状态：刚插线尚未点「允许 USB 调试」时是
// unauthorized，也应列出（前端提示用户去手机授权）。
func (a *ADB) UsbSerials(ctx context.Context) ([]string, error) {
	out, err := a.run(ctx, "devices")
	if err != nil {
		return nil, err
	}
	return parseUsbSerials(out), nil
}

// parseUsbSerials 从 `adb devices` 输出中提取 USB 有线设备的序列号。
// 规则：serial 不含冒号（无线设备是 ip:port 含冒号），且状态为 device 或
// unauthorized（offline 的忽略，因为那是连接异常，非正常待切换状态）。
func parseUsbSerials(out string) []string {
	var serials []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		serial, state := fields[0], fields[1]
		if strings.Contains(serial, ":") {
			continue // 跳过无线设备（ip:port）
		}
		if state == "device" || state == "unauthorized" {
			serials = append(serials, serial)
		}
	}
	return serials
}

// MdnsServices 返回 mDNS 发现的「无线调试」服务列表，每项形如 "192.168.1.100:35583"。
// 依赖 `adb mdns services`（adb 31.0.2+ 默认开启），用于设备重启后 IP/端口都变化
// 时自动发现设备当前的真实连接地址。
func (a *ADB) MdnsServices(ctx context.Context) ([]string, error) {
	out, err := a.run(ctx, "mdns", "services")
	if err != nil {
		return nil, err
	}
	return parseMdnsServices(out), nil
}

// parseMdnsServices 从 `adb mdns services` 输出中提取 ip:port 列表。
// 输出形如：
//
//	List of discovered mdns services
//	adb-ZY22L4X5X7-qurAZn	_adb-tls-connect._tcp	192.168.41.177:35583
//
// 只提取「无线调试」的 TLS 连接服务（_adb-tls-connect._tcp），这是配对后
// 用于建立连接的动态端口；忽略其它服务类型。
func parseMdnsServices(out string) []string {
	var addrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// 需要至少 3 列：服务名、服务类型、地址。
		if len(fields) < 3 {
			continue
		}
		if fields[1] != "_adb-tls-connect._tcp" {
			continue
		}
		addrs = append(addrs, fields[2])
	}
	return addrs
}

// Pair 执行无线调试配对。pairPort 是设备「无线调试」页显示的配对端口，
// code 是 6 位配对码。
//
// 重要：adb pair 只以退出码 0 表示配对成功。配对失败（IP 错误、端口错误、
// 配对码错误、设备未响应）时，adb 会以退出码 1 返回，且 stderr 统一是
// "protocol fault (couldn't read status message)"——这个错误同时也会出现在
// 「对已配对设备重复配对」的场景，因此无法据此区分「已配对」与「真失败」。
// 所以这里不做任何文本兜底，只有退出码 0 才算成功，其余一律返回错误，
// 避免把失败误报为「配对成功」。
func (a *ADB) Pair(ctx context.Context, host string, pairPort int, code string) error {
	_, err := a.run(ctx, "pair", fmt.Sprintf("%s:%d", host, pairPort), code)
	return err
}

// Connect 建立无线调试连接。port 是设备「无线调试」页显示的连接端口（非配对端口）。
// adb connect 命令即使目标不在线也可能返回成功（状态为 offline/unauthorized），
// 因此 connect 后轮询等待设备进入 device 状态，超时则返回明确错误，避免上层
// 误以为已连接成功。
func (a *ADB) Connect(ctx context.Context, host string, port int) error {
	if _, err := a.run(ctx, "connect", fmt.Sprintf("%s:%d", host, port)); err != nil {
		return err
	}
	serial := fmt.Sprintf("%s:%d", host, port)
	// 设备从 connect 到真正进入 device 状态需要几秒，轮询等待。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		online, err := a.IsDeviceOnline(ctx, serial)
		if err != nil {
			return err
		}
		if online {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	// 超时仍未 device：区分 unauthorized（未授权）与 offline（掉线）给出更明确提示。
	out, _ := a.run(ctx, "devices")
	if strings.Contains(out, serial+"\tunauthorized") {
		return fmt.Errorf("device %s unauthorized（请在手机上确认允许 USB 调试/无线调试授权）", serial)
	}
	return fmt.Errorf("device %s not online after connect (adb devices 中无 device 状态，可能无线调试已关闭或端口已变)", serial)
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

// Tcpip 把一台通过 USB 连接的设备（serial 为设备序列号）切换到 TCP 监听模式，
// 供拔线后走 Wi-Fi 连接（adb connect ip:port）。适用于没有「无线调试」菜单的
// Android 11 及更早设备。port 通常为 5555。
func (a *ADB) Tcpip(ctx context.Context, serial string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid tcpip port %d", port)
	}
	_, err := a.run(ctx, "-s", serial, "tcpip", strconv.Itoa(port))
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
	_, err := a.runWithTimeout(ctx, adbTransferTimeout, "-s", serial, "push", localPath, remotePath)
	return err
}

// Pull 把设备上的文件或目录拉取到本地指定路径。
func (a *ADB) Pull(ctx context.Context, serial, remotePath, localPath string) error {
	_, err := a.runWithTimeout(ctx, adbTransferTimeout, "-s", serial, "pull", remotePath, localPath)
	return err
}

// Install 把本地 APK 安装到指定设备。localAPK 是服务器上的 APK 文件路径。
//
// 使用 `adb install -r -g`：-r 覆盖安装（同版本或降级也能装），-g 自动授予
// 运行时权限（避免装完后因缺权限导致应用不可用，符合批量部署场景）。
// 安装耗时与 APK 大小线性相关（几十 MB 经 Wi-Fi 需十几秒），用 adbTransferTimeout。
func (a *ADB) Install(ctx context.Context, serial, localAPK string) error {
	_, err := a.runWithTimeout(ctx, adbTransferTimeout, "-s", serial, "install", "-r", "-g", localAPK)
	return err
}

// Uninstall 从指定设备卸载应用。packageName 是应用包名（如 com.example.app），
// 非 APK 文件名。卸载为一次性 shell 操作，用默认超时即可。
func (a *ADB) Uninstall(ctx context.Context, serial, packageName string) error {
	_, err := a.run(ctx, "-s", serial, "uninstall", packageName)
	return err
}

// PullStream 通过 `adb exec-out cat` 把设备文件的内容流式写入 w，边拉边写。
//
// 与 Pull（先把整个文件拉取到本地落盘、再一次性读取）不同，exec-out 从第一条
// 字节起就持续产出，直接把内容交给调用方（HTTP 响应体）。这解决大文件下载时
// 「响应头已发、但 body 迟迟不出现」导致浏览器判定连接异常而中断的问题。
//
// 代价：若拉取中途设备断连，错误发生时数据可能已部分写入 w，无法撤回；调用方
// 已下发的 Content-Length 与最终字节数可能不一致，浏览器会得到截断文件。这是
// 流式传输的固有取舍，比「先落盘再一次性读」更契合实时下载场景。
func (a *ADB) PullStream(ctx context.Context, serial, remotePath string, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, adbTransferTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, a.path, "-s", serial, "exec-out", "cat", shellQuote(remotePath))
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("adb exec-out cat %s: %s", remotePath, msg)
	}
	return nil
}

// DeviceFile 是设备上一个文件/目录的最小信息，供文件浏览界面展示。
type DeviceFile struct {
	Name    string // 文件或目录名（不含路径）
	IsDir   bool   // 是否目录
	Size    int64  // 字节数；目录为 0
	IsImage bool   // 是否常见图片类型（供前端决定是否请求缩略图）
}

// ListFiles 列出设备上指定目录的内容。
//
// 实现用 `adb shell ls -la <path>` 并解析输出。相比 `ls -l`，`-a` 会带上
// 隐藏文件，但这里刻意跳过 `.` 与 `..` 两个特殊项；解析依赖 -l 的固定列，
// 文件名中的空格会导致字段数变化，故从第 8 列开始按「权限列固定 10 字符」
// 拆分，剩余部分视为文件名（可含空格）。
func (a *ADB) ListFiles(ctx context.Context, serial, path string) ([]DeviceFile, error) {
	out, err := a.Shell(ctx, serial, "ls -la "+shellQuote(path))
	if err != nil {
		return nil, err
	}
	return parseLsOutput(out), nil
}

// shellQuote 给 adb shell 里的路径参数加单引号，避免空格/特殊字符被拆词。
// 路径中若本身含单引号则退回不转义（极罕见，宁可报错也不注入）。
func shellQuote(s string) string {
	if strings.Contains(s, "'") {
		return s
	}
	return "'" + s + "'"
}

// parseLsOutput 解析 `ls -la` 的输出为文件列表。
//
// 每一行形如（toybox 默认日期为 YYYY-MM-DD）：
//
//	drwxr-xr-x 2 root root  4096 2024-01-01 12:00 Camera
//	-rw-r--r-- 1 root root 2413 2024-01-01 12:00 IMG_001.jpg
//
// 首列首字符 'd' 表示目录；'-' 表示普通文件。第 5 列（索引 4）是大小，
// 目录/符号链接的大小列无意义，只对普通文件取 size。文件名从日期/时间字段
// 之后开始拼接（含空格的文件名也能完整保留）。
//
// 日期字段数因 ls 实现而异：toybox 用 `YYYY-MM-DD HH:MM`（2 个字段），
// GNU/coreutils 老格式用 `MMM DD HH:MM`（3 个字段）。因此先看第 6 个字段
// 是否为 `YYYY-MM-DD` 形式来判定文件名起始索引，避免硬编码列数。
func parseLsOutput(out string) []DeviceFile {
	lines := strings.Split(out, "\n")
	var files []DeviceFile
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue // 无法确定文件名的行跳过
		}
		mode := fields[0]
		if len(mode) == 0 {
			continue
		}
		isDir := mode[0] == 'd'
		// 文件名起始索引：默认 7（YYYY-MM-DD 日期占 2 字段：date + time），
		// 若第 6 字段不是 YYYY-MM-DD，则日期占 3 字段，名称从 8 开始。
		nameIdx := 7
		if !isYYYYMMDD(fields[5]) {
			nameIdx = 8
		}
		if nameIdx >= len(fields) {
			continue
		}
		name := strings.Join(fields[nameIdx:], " ")
		if name == "." || name == ".." {
			continue
		}
		var size int64
		if !isDir {
			// 第 5 列（索引 4）是大小，解析失败时保留 0。
			if n, err := strconv.ParseInt(fields[4], 10, 64); err == nil {
				size = n
			}
		}
		files = append(files, DeviceFile{
			Name:    name,
			IsDir:   isDir,
			Size:    size,
			IsImage: isImageName(name),
		})
	}
	return files
}

// isYYYYMMDD 判断字段是否为 YYYY-MM-DD 形式的日期，用于区分 ls 的日期列宽。
func isYYYYMMDD(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isImageName 根据扩展名判断是否为常见图片类型，供缩略图功能使用。
func isImageName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return true
	}
	return false
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
