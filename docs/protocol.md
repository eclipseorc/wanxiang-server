# 万象 WebSocket 通信协议

所有 WebSocket 消息均为包含 `type` 字段的 JSON 对象。设备端与管理端均通过各自 HTTP 服务换取一次性短期票据（`ticket`，60 秒有效、用后即删）完成握手，后端共享同一个 SQLite 数据库。

## 1. 设备端生命周期（`/ws/device`）

1. **换票与登记**：App 本地生成随机 `deviceId`，请求 `POST /api/device/ws-ticket`（`{"deviceId":"...","model":"..."}`），首次调用自动登记设备。
2. **建连**：使用返回的一次性票据连接 `/ws/device?ticket=<ticket>`。
3. **周期心跳**：设备定时上报：
   ```json
   {"type":"heartbeat","battery":80,"rttMs":30,"status":"ready","ip":"192.168.1.100"}
   ```
   - 服务端回执 `heartbeat-ack` 并向有权限的管理端广播 `device-state`。
   - `ip` 为设备当前 Wi-Fi IPv4 地址；当检测到局域网 IP 变化时，服务端自动同步数据库 `adb_ip`、广播 `device-adb-state` 并异步重连新 IP。
4. **ADB 自动配对与就绪上报**：
   - `adb-pair-request`（设备 → 服务端）：无障碍读取 6 位配对码后发给服务端执行 `adb pair` + `adb connect` + `pm grant WRITE_SECURE_SETTINGS`，服务端回执 `adb-pair-ack`。
   - `adb-ready`（设备 → 服务端）：携带 `lan_ip`、`adb_port`、`adb_mode`（`wireless` / `tcpip`），触发服务端执行 `adb connect` 并回执 `adb-ready-ack`。
5. **无障碍控件树抓取**：服务端下发 `{"type":"dump-ui-tree","reqId":"..."}`，设备返回 `{"type":"ui-tree-result","reqId":"...","ok":true,"nodes":[...]}`（单帧上限 `512KB`）。
6. **断开与禁用**：连接关闭标记离线并广播 `device-offline`；管理员禁用设备时下发 `disconnect` 并断开连接。

## 2. 管理端生命周期（`/ws/admin`）

1. 通过 HTTP 登录后调用 `POST /api/admin/ws-ticket` 换票，连接 `/ws/admin?ticket=<ticket>`。
2. 建连首帧下发按 `Store.CanAccess` 过滤的 `device-list` 快照（在线态以内存 `hub.devices` 为准）。
3. 后续实时推送 `device-online`、`device-offline`、`device-state`、`device-adb-state`、`device-admin-state`。
4. 权限模型：管理员可见全部设备；普通用户仅可见其所属用户组通过 `group_access` 获授 `can_view` / `can_manage` 的设备组内设备。
5. 远程画面与控制由独立的 `scrcpy-server`（`:9004`）通过 ADB 桥接 H.264 视频与触控事件，不经 WebSocket 转发进程。

## 3. `com` 应用层命令信封

应用层命令统一使用 `type: "com"` 信封，采用白名单分发，未知命令统一拒绝（与底层 WebSocket Ping/Pong 控制帧独立）：

- **管理端心跳探测**：
  ```json
  {"type":"com","command":"ping","requestId":"ping-1731670000000-1","data":{"sentAt":1731670000000}}
  ```
  服务端原样保留 `requestId` 与 `sentAt` 返回 `command: "pong"`。
- **ADB 状态通知（服务端 → 设备）**：
  ```json
  {"type":"com","command":"adb-status","data":{"connected":false,"ip":"192.168.1.100"}}
  ```
- **错误响应**：
  ```json
  {"type":"com","command":"error","requestId":"...","data":{"code":"unsupported_command","message":"unsupported command"}}
  ```
- **扩展约定**：所有 WebSocket 入站消息与 `com` 命令均统一注册在 `internal/transport/websocket/commands.go`（设备端 `deviceRoutes`、管理端 `adminRoutes`、`com` 信封 `comHandlers`），新增命令只需在对应注册表加一行并补充单测，无需改动 `server.go`；涉及状态变更的命令必须校验角色与 `CanAccess` 权限。
