package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
)

// AdbConnector 是「adb 连接管理」能力的唯一收口点，供 admin-server 与
// websocket-server 复用，避免 transport 层各自直接抓取 scrcpy.ADB 造成职责分散。
//
// 它承载两类职责：
//  1. 连接管理的薄代理（下方 Pair/Connect/Tcpip/Disconnect/OnlineDevices 等）——
//     原样转发 scrcpy.ADB 的同名方法，不改变任何语义，只是把「连接管理」这组
//     写/读操作从 transport 层收敛到 logic 层统一入口；
//  2. MDM 路径的业务动作（ConnectAfterReady）——封装「设备本地自配对完成后，
//     服务端查授权并 connect」的完整链路。
//
// 注意：AdbConnector 只收口「连接管理」（把设备拉进/移出 adb 连接列表）相关方法；
// 文件/会话类操作（Forward/Push/Pull/Install/Shell/ListFiles/PullStream 等）仍由
// scrcpy.ADB 直接提供，供 scrcpy-server 与 adminapi 的文件/APK 端点使用，不在此收口。
// AdbConnector 也不负责管理员鉴权——那是 transport 层的事。
type AdbConnector struct {
	svc *Service
	adb *scrcpy.ADB
}

// --- 连接管理薄代理（原样转发 scrcpy.ADB，不改变语义） ---

// EnsureServer 启动 adb server（若未运行）。
func (c *AdbConnector) EnsureServer(ctx context.Context) error {
	return c.adb.EnsureServer(ctx)
}

// Pair 执行无线调试配对。
func (c *AdbConnector) Pair(ctx context.Context, host string, pairPort int, code string) error {
	return c.adb.Pair(ctx, host, pairPort, code)
}

// Connect 建立无线调试连接（内部已轮询等待 device 状态）。
func (c *AdbConnector) Connect(ctx context.Context, host string, port int) error {
	return c.adb.Connect(ctx, host, port)
}

// ResolveOnlinePort 返回指定 IP 当前在 adb 中处于 device 状态的连接端口。
func (c *AdbConnector) ResolveOnlinePort(ctx context.Context, host string) (int, error) {
	return c.adb.ResolveOnlinePort(ctx, host)
}

// IsDeviceOnline 判断指定 serial 是否处于 device（在线可操作）状态。
func (c *AdbConnector) IsDeviceOnline(ctx context.Context, serial string) (bool, error) {
	return c.adb.IsDeviceOnline(ctx, serial)
}

// IsPaired 判断指定 serial 是否已出现在 adb 设备列表中（含 unauthorized 视为已配对）。
func (c *AdbConnector) IsPaired(ctx context.Context, serial string) (present bool, online bool, unauthorized bool, err error) {
	return c.adb.IsPaired(ctx, serial)
}

// UsbSerials 返回 adb 中通过 USB 有线连接的设备序列号列表。
func (c *AdbConnector) UsbSerials(ctx context.Context) ([]string, error) {
	return c.adb.UsbSerials(ctx)
}

// OnlineDevices 返回 adb 中处于 device 状态的设备 serial 列表。
func (c *AdbConnector) OnlineDevices(ctx context.Context) ([]string, error) {
	return c.adb.OnlineDevices(ctx)
}

// MdnsServices 返回 mDNS 发现的「无线调试」服务列表（ip:port）。
func (c *AdbConnector) MdnsServices(ctx context.Context) ([]string, error) {
	return c.adb.MdnsServices(ctx)
}

// Disconnect 断开与指定 serial 的连接。
func (c *AdbConnector) Disconnect(ctx context.Context, serial string) error {
	return c.adb.Disconnect(ctx, serial)
}

// Tcpip 把一台 USB 连接的设备切换到 TCP 监听模式（adb tcpip <port>）。
func (c *AdbConnector) Tcpip(ctx context.Context, serial string, port int) error {
	return c.adb.Tcpip(ctx, serial, port)
}

// NewAdbConnector 创建一个 adb 连接器。adb 为 nil 时表示未配置 adb 二进制，
// ConnectAfterReady 会仅落库并返回未连接（connected=false），不报错、不触发 connect。
func NewAdbConnector(svc *Service, adb *scrcpy.ADB) *AdbConnector {
	return &AdbConnector{svc: svc, adb: adb}
}

// ConnectAfterReady 处理设备「ADB 就绪」事件：
//  1. 登记设备（幂等，可能先于心跳/换票到达）；
//  2. 落库 AdbIP/AdbPort 与在线状态；
//  3. 若未配置 adb，仅落库并返回未连接；
//  4. 先查是否已配对（IsPaired），已配对才 connect，避免对未授权设备反复 connect；
//  5. connect 成功后标记 adb 在线。
//
// deviceID、ip 为必填；port<=0 时默认 5555。返回 connected 表示设备是否已进入
// adb 在线（device）状态；未连接（未配对/未授权/未配置）时返回 connected=false
// 且 err 为 nil，真正的执行失败才返回 error。
func (c *AdbConnector) ConnectAfterReady(ctx context.Context, deviceID, ip string, port int) (bool, error) {
	deviceID = strings.TrimSpace(deviceID)
	ip = strings.TrimSpace(ip)
	if deviceID == "" || ip == "" {
		return false, fmt.Errorf("device_id and lan_ip are required")
	}
	if port <= 0 {
		port = 5555
	}

	// 先登记设备（adb-ready 可能先于心跳/换票到达），再回填 adb 地址。
	if err := c.svc.RegisterDevice(deviceID, ""); err != nil {
		return false, err
	}
	if err := c.svc.Store.SetOnline(deviceID, true); err != nil {
		return false, err
	}

	// 先落库设备上报的 IP/端口与自动模式（无论后续 connect 是否成功，都保留最新地址与模式）。
	if err := c.svc.Store.SetDeviceAdbWithMode(deviceID, ip, port, false, "auto"); err != nil {
		return false, err
	}

	if c.adb == nil {
		return false, nil
	}

	_ = c.adb.EnsureServer(ctx)
	serial := ip + ":" + strconv.Itoa(port)

	// 检查当前 adb 列表状态：已 online 则无需重复 connect；处于 unauthorized
	// 说明已发起过连接、正在等待手机上点击 RSA 授权，不重复 connect；
	// 其余情况（首次连接 !present 或 offline）立即执行 adb connect 拉上线。
	_, online, unauthorized, err := c.adb.IsPaired(ctx, serial)
	if err != nil {
		return false, err
	}
	if unauthorized {
		// 已连接但尚未在设备上点「允许」，提示等待授权，不反复 connect。
		return false, nil
	}
	if online {
		// 已经在线，无需重复 connect。
		if err := c.svc.Store.SetDeviceAdbWithMode(deviceID, ip, port, true, "auto"); err != nil {
			return false, err
		}
		return true, nil
	}

	// 首次收到 adb-ready（!present）或处于 offline，立即执行 connect 拉上线。
	if err := c.adb.Connect(ctx, ip, port); err != nil {
		return false, err
	}
	if err := c.svc.Store.SetDeviceAdbWithMode(deviceID, ip, port, true, "auto"); err != nil {
		return false, err
	}
	_ = c.svc.Store.Audit("mdm", "mdm-adb-ready", deviceID, serial)
	return true, nil
}
