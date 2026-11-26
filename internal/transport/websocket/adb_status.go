// adb_status.go 负责「业务在线但 ADB 离线」的检测与通知。
//
// 背景：远程控制依赖 adb 无线连接，而设备的业务 WebSocket 心跳与 adb 连接
// 是两套独立机制。设备重启/无线调试被系统关闭后，业务 WS 会自动重连（设备
// 显示「在线」），但 adb 连接不会自动恢复。本文件周期性扫描 adb 在线设备，
// 找出「业务在线但 adb 离线」的设备，通过设备连接下发 adb-status 消息，
// 引导 App 弹通知并跳转系统「无线调试」页，实现半自动恢复。
package websocket

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
)

// adbStatusScanInterval 是扫描 adb 在线状态的周期。周期不宜过短，
// 因为 adb devices 每次调用都有进程开销；30 秒足以在设备掉线后及时提醒。
const adbStatusScanInterval = 30 * time.Second

// adbReconnectPort 是传统模式（USB 切 tcpip）设备的固定监听端口，仅作为
// 无线调试设备无落库端口时的兜底重连端口。无线调试设备不能依赖此端口，
// 应优先用落库的真实端口（adb_port）重连。
const adbReconnectPort = 5555

// adbReconnectMinInterval 是同一设备两次自动重连尝试之间的最小间隔，避免
// 设备确实已重启（5555 未监听）时每 30 秒就重复 connect 一次造成无谓开销。
// 取 45 秒：设备重启后 Shizuku 自启重开无线调试 + mDNS 广播约需 1~2 分钟，
// 45 秒限频能让 mDNS 一旦发现新端口就能在下一个扫描周期（30 秒）内重连成功，
// 又不会在设备离线空窗期过于频繁地空 connect。
const adbReconnectMinInterval = 45 * time.Second

// ADBMonitor 周期扫描 adb，向「业务在线但 adb 离线」的设备下发 adb-status。
type ADBMonitor struct {
	hub *Hub
	// connector 是「连接管理」的统一入口（logic.AdbConnector），承载在线设备
	// 探测（OnlineDevices/MdnsServices）与自动重连（Connect），与 admin-server
	// 共享同一份实现，避免 adb 写操作散落在多个 transport。
	connector *logic.AdbConnector

	// lastAdbState 记录每个 deviceId 上一次的 adb 连接状态，只在状态变化时
	// 下发通知，避免每 30 秒重复轰炸设备。
	lastAdbState map[string]bool
	// lastReconnect 记录每个 deviceId 上一次自动重连尝试的时间，用于限频。
	lastReconnect map[string]time.Time
}

// NewADBMonitor 创建扫描器。connector 为 nil 时（未配置 adb）会静默停用，
// 避免进程因 adb 缺失而崩溃；日志会提示远程控制相关能力不可用。
func NewADBMonitor(hub *Hub, connector *logic.AdbConnector) *ADBMonitor {
	return &ADBMonitor{
		hub:           hub,
		connector:     connector,
		lastAdbState:  make(map[string]bool),
		lastReconnect: make(map[string]time.Time),
	}
}

// Start 启动后台扫描循环，直到 ctx 取消。
func (m *ADBMonitor) Start(ctx context.Context) {
	if m.connector == nil {
		log.Printf("adb-status monitor disabled: adb not available")
		return
	}
	go m.loop(ctx)
}

// loop 周期性扫描 adb，对比业务在线设备，对状态变化下发通知。启动时立即扫一次。
func (m *ADBMonitor) loop(ctx context.Context) {
	m.scanOnce()
	ticker := time.NewTicker(adbStatusScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.scanOnce()
		}
	}
}

// scanOnce 执行一次扫描：拿到 adb 在线 serial 列表与 mDNS 发现的服务，
// 对比业务在线设备的 IP，找出「业务在线但 adb 离线」的设备并尝试自动恢复，
// 恢复不了再下发 adb-status 引导人工。
func (m *ADBMonitor) scanOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	onlineSerials, err := m.connector.OnlineDevices(ctx)
	if err != nil {
		log.Printf("adb-status scan failed: %v", err)
		return
	}

	// mDNS 发现（设备重启后 IP/端口都变时用）。失败不阻断主流程，只降级为
	// 仅靠固定端口重连 + 人工通知。
	mdnsByIP := map[string]string{}
	if services, err := m.connector.MdnsServices(ctx); err == nil {
		mdnsByIP = mdnsAddrByIP(services)
	} else {
		log.Printf("adb-status mdns scan failed: %v", err)
	}

	// 快照当前业务在线设备及其上报 IP。
	type deviceInfo struct {
		id string
		ip string
	}
	m.hub.mu.RLock()
	infos := make([]deviceInfo, 0, len(m.hub.devices))
	for id, ip := range m.hub.deviceIPs {
		// 只关注当前业务在线的设备（devices 里存在才是业务在线）。
		if _, ok := m.hub.devices[id]; ok {
			infos = append(infos, deviceInfo{id: id, ip: ip})
		}
	}
	m.hub.mu.RUnlock()

	onlineIPs := serialIPSet(onlineSerials)
	for _, info := range infos {
		// 无 IP 上报的设备无法判断 adb 状态，跳过。
		if info.ip == "" {
			continue
		}
		// 以 hub.adbOnline 作为单一真相源，避免 handleAdbReady / syncDeviceIPChange 更新状态后
		// 与 lastAdbState 不同步导致漏发离线事件。
		m.hub.mu.RLock()
		hubOnline, hubKnown := m.hub.adbOnline[info.id]
		m.hub.mu.RUnlock()

		connected := onlineIPs[info.ip]
		if connected {
			if prev, known := m.lastAdbState[info.id]; !known || !prev || !hubKnown || !hubOnline {
				m.publishState(info.id, info.ip, true)
			}
			continue
		}

		// 业务在线但 adb 离线：先尝试自动重连（固定端口 + mDNS），成功则恢复，
		// 失败才通知人工。
		if m.shouldTryReconnect(info.id) {
			m.lastReconnect[info.id] = time.Now()
			if m.tryRecover(info.id, info.ip, mdnsByIP) {
				continue
			}
		}

		// 自动重连失败（或未到重试时机）：标记离线并通知人工。
		if prev, known := m.lastAdbState[info.id]; !known || prev || (hubKnown && hubOnline) {
			m.publishState(info.id, info.ip, false)
		}
	}
}

// tryRecover 尝试自动恢复一台 adb 离线设备。始终使用设备最新心跳上报的 ip，
// 绝不去连已经失效的旧 dev.AdbIP（否则 TCP 超时会直接耗光整个恢复时间窗口）。
// 优先用落库的真实端口重连，其次尝试 5555，最后用 mDNS 发现的动态端口连接。
func (m *ADBMonitor) tryRecover(deviceID, ip string, mdnsByIP map[string]string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 1) 落库的真实端口（若与 5555 不同，用最新 ip + 该端口先试）。
	if dev, err := m.hub.store.DeviceByID(deviceID); err == nil && dev.AdbPort > 0 && dev.AdbPort != adbReconnectPort {
		if m.connector.Connect(ctx, ip, dev.AdbPort) == nil {
			log.Printf("adb-status auto reconnect ok device_id=%s ip=%s port=%d", deviceID, ip, dev.AdbPort)
			m.publishStateWithPort(deviceID, ip, dev.AdbPort, true)
			return true
		}
	}

	// 2) 固定 5555 端口（无线自配对固化 5555 或传统模式 tcpip 5555）。
	if m.connector.Connect(ctx, ip, adbReconnectPort) == nil {
		log.Printf("adb-status auto reconnect ok device_id=%s ip=%s port=%d", deviceID, ip, adbReconnectPort)
		m.publishStateWithPort(deviceID, ip, adbReconnectPort, true)
		return true
	}

	// 3) mDNS 发现的动态端口（设备重启或切网后端口变了）。连上后保持原端口，
	//    不再切回 5555（无线调试设备切 5555 会破坏授权）。
	if addr, ok := mdnsByIP[ip]; ok {
		host, portStr, ok := strings.Cut(addr, ":")
		if ok {
			if port, err := strconv.Atoi(portStr); err == nil {
				if err := m.connector.Connect(ctx, host, port); err == nil {
					log.Printf("adb-status mdns reconnect ok device_id=%s addr=%s", deviceID, addr)
					m.publishStateWithPort(deviceID, host, port, true)
					return true
				}
			}
		}
	}

	log.Printf("adb-status auto reconnect failed device_id=%s ip=%s", deviceID, ip)
	return false
}

// mdnsAddrByIP 把 mDNS 服务地址列表（"ip:port"）转成 IP → 完整地址的映射，
// 供按设备上报 IP 查找其当前动态端口。同 IP 多个地址时保留最后一个。
func mdnsAddrByIP(addrs []string) map[string]string {
	out := make(map[string]string, len(addrs))
	for _, addr := range addrs {
		if ip, _, ok := strings.Cut(addr, ":"); ok {
			out[ip] = addr
		}
	}
	return out
}

// shouldTryReconnect 判断是否到了对该设备再次自动重连的时机（限频）。
func (m *ADBMonitor) shouldTryReconnect(deviceID string) bool {
	last, ok := m.lastReconnect[deviceID]
	if !ok {
		return true
	}
	return time.Since(last) >= adbReconnectMinInterval
}

// serialIPSet 把 adb 在线 serial 列表（形如 "192.168.1.100:5555"）转换为
// 去重后的 IP 集合，供与设备上报 IP 做匹配。
func serialIPSet(serials []string) map[string]bool {
	set := make(map[string]bool, len(serials))
	for _, serial := range serials {
		if ip, _, ok := strings.Cut(serial, ":"); ok {
			set[ip] = true
		}
	}
	return set
}

// publishState 统一收口「ADB 状态变化」时的三件事：
//  1. 更新 lastAdbState（避免重复通知）；
//  2. 更新 hub.adbOnline（供管理端快照与广播读取）；
//  3. 下发 adb-status 给设备 + 广播 device-adb-state 给管理端。
func (m *ADBMonitor) publishState(deviceID, ip string, connected bool) {
	m.publishStateWithPort(deviceID, ip, 0, connected)
}

func (m *ADBMonitor) publishStateWithPort(deviceID, ip string, knownPort int, connected bool) {
	m.lastAdbState[deviceID] = connected

	m.hub.mu.Lock()
	m.hub.adbOnline[deviceID] = connected
	m.hub.mu.Unlock()

	// 通知设备端 App（弹/撤通知）。
	m.hub.mu.RLock()
	device := m.hub.devices[deviceID]
	m.hub.mu.RUnlock()
	if device != nil {
		msg := map[string]any{
			"type":    "com",
			"command": "adb-status",
			"data": map[string]any{
				"connected": connected,
				"ip":        ip,
			},
		}
		if err := device.send(msg); err != nil {
			log.Printf("adb-status notify failed device_id=%s: %v", deviceID, err)
		}
	}

	// 广播给管理端（前端据此标注「ADB 掉线」或「在线」状态，同时同步 IP、端口与模式）。
	port := knownPort
	mode := ""
	if dev, err := m.hub.store.DeviceByID(deviceID); err == nil {
		if port <= 0 && dev.AdbPort > 0 {
			port = dev.AdbPort
		}
		mode = strings.TrimSpace(dev.AdbMode)
	}
	if port <= 0 {
		port = adbReconnectPort
	}
	if mode == "" {
		mode = "wireless"
	}
	if connected {
		_ = m.hub.store.SetDeviceAdbWithMode(deviceID, ip, port, true, mode)
	} else {
		_ = m.hub.store.SetDeviceAdb(deviceID, ip, port, false)
	}
	m.hub.broadcastDevice(deviceID, "device-adb-state", map[string]any{
		"adbOnline": connected,
		"ip":        ip,
		"port":      port,
		"adbMode":   mode,
	})

	state := "online"
	if !connected {
		state = "offline"
	}
	log.Printf("adb-status published device_id=%s ip=%s:%d adb=%s", deviceID, ip, port, state)
}
