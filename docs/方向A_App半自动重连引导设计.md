# 方向 A 设计：App 半自动重连引导（ADB 掉线恢复）

本文档是「设备无线接入方案」的落地设计之一，回答一个问题：**设备 ADB 掉线后，如何用最少的人工动作恢复远程控制？**

结论先行（已在《设备无线接入方案》中论证）：未 root 下无法做到「全自动」，唯一可落地的目标是「半自动」——用户只需「点一下通知 + 打开无线调试开关」，其余（发现、连接、切固定端口）交给 App 与后端自动化。

---

## 1. 目标与边界

### 1.1 目标

设备因重启 / 掉网 / 无线调试被系统关闭而 ADB 掉线后：

1. App 能**感知**「ADB 已掉线」。
2. App **弹通知**引导用户恢复。
3. 用户点通知 → App **一键跳转**系统「无线调试」设置页。
4. App **上报本机 IP** 给后端。
5. 后端 **mDNS 自动发现 + 自动 connect + 自动切 5555**，恢复远程控制。

### 1.2 边界（未 root 硬约束）

- App **无法自动打开**「无线调试」开关（需 `WRITE_SECURE_SETTINGS`，普通 App 无权限）。
- App **无法读取配对码**（配对码由 adbd 生成，仅显示在系统 UI；首次配对必须人眼看一次）。
- 因此，**唯一无法消除的人工动作 = 用户在手机上打开「无线调试」开关**；首次接入时还需看一次配对码。

---

## 2. 核心问题：App 如何「检测 ADB 掉线」？

App 跑在**设备内部**，无法直接看到服务器端的 `adb devices` 状态。而 ADB 连接的 serial（`IP:端口`）是**服务器侧 adb** 维护的，App 无从知晓自己的 serial，因此无法主动精确探测。

**采用「后端主动下发」方案**（推荐，复用现有通道，零新增基础设施）：

- 后端（scrcpy-server 或 admin-server）周期性扫描 `adb devices`。
- 对「业务在线（WS 心跳正常）但 ADB 离线（不在 `device` 状态）」的设备，通过**已有的设备端业务 WebSocket**（`/ws/device`）下发一条消息。
- App 收到后弹通知。

### 为什么选后端下发而不是 App 轮询

| 方案 | 问题 |
| --- | --- |
| App 轮询 | App 不知道自己的 adb serial（IP:端口），无法精确查询；且需建立「deviceId ↔ serial」映射 |
| 后端下发（推荐） | 后端天然掌握「业务在线状态 + adb 在线状态」两边的数据，判断「掉线」最准确，且复用现有 WS 通道 |

---

## 3. 关键前提：建立「deviceId ↔ adb serial」映射

后端要判断「某台业务设备对应的 adb 连接是否掉线」，必须先知道**哪个 deviceId 对应哪个 adb serial**。当前系统里这个映射是缺失的（deviceId 由 App 生成，serial 由 adb 维护，两者无关联）。

### 3.1 映射方案：App 上报本机 IP

App 能读自己的 Wi-Fi IP（无需特殊权限），而 adb 无线设备的 serial 就是 `IP:端口`。因此：

1. App 在心跳（或单独上报）时附带本机 IP：`{"ip":"192.168.1.100"}`。
2. 后端据此知道「deviceId ↔ IP」。
3. 后端扫 `adb devices` 时，用 IP 前缀匹配：`adb devices` 里的 `192.168.1.100:xxxxx` 属于该 deviceId。
4. 若某 deviceId 的业务 WS 在线，但 `adb devices` 里没有以该 IP 开头的 device 状态项 → 判定「ADB 掉线」。

> 注意：IP 可能变化（路由器重启重分配）。所以这个映射要**每次心跳动态刷新**，不能只记一次。mDNS 兜底也能缓解 IP 变化（见 §5）。

### 3.2 需要的字段

设备表 `devices` 当前无 IP 字段。需要新增（additive 迁移）：

- `last_ip`（TEXT，可空）：App 最近一次上报的本机 IP。

或更轻量：不落库，纯内存态维护「deviceId → 最近 IP」映射（因为 IP 变化频繁，落库意义不大，且掉线判断是实时的）。**建议先用内存态**，避免不必要的 DB 迁移。

---

## 4. 端到端流程

### 4.1 正常在线（无动作）

```
App 心跳（带 IP）→ 后端更新 deviceId→IP 映射
后端扫 adb devices → 发现该 IP 在 device 状态 → 一切正常，不下发通知
```

### 4.2 设备掉线（触发半自动恢复）

```
1. 设备重启 / 无线调试被系统关闭 → adb 连接断开
2. App 前台服务 START_STICKY 自动重启 → 业务 WS 重连成功 → 心跳恢复（带 IP）
3. 后端扫 adb devices → 发现「业务在线但该 IP 不在 device 状态」→ 判定 ADB 掉线
4. 后端通过业务 WS 下发 adb-status 消息给该设备：
   {"type":"com","command":"adb-status","requestId":"...","data":{"connected":false,"ip":"192.168.1.100"}}
5. App 收到 → 弹系统通知「远程控制已断开，点此恢复」
6. 用户点通知 → App 启动引导页（或直接跳转）
7. 引导页显示「请在开发者选项中打开无线调试」+「去设置」按钮
8. 用户点「去设置」→ App 跳转系统无线调试页（ACTION_APPLICATION_DEVELOPMENT_SETTINGS）
9. 用户打开「无线调试」开关（唯一人工动作）
10. App 继续周期上报 IP；后端 mDNS 自动发现 → connect → tcpip 5555
11. 远程控制恢复 → 后端下发 adb-status connected=true → App 撤销通知
```

### 4.3 首次接入（需看配对码）

```
用户首次接入：App 跳转无线调试页 → 用户打开开关 → 看配对码 + IP + 端口
→ 用户在管理后台手动录入配对（一次）
→ 之后进入 4.1/4.2 的自动流程
```

---

## 5. 后端改动清单

### 5.1 新增「ADB 掉线扫描」任务（放在 websocket-server）

- 周期（如每 30s）执行 `adb devices`，得到在线 serial 列表（已有 `OnlineDevices()`）。
- 结合「业务在线设备 + 其 IP」判断哪些设备「业务在线但 ADB 离线」。
- 对掉线设备，直接通过本进程的设备连接 hub 向其 deviceId 下发 `adb-status` 消息。

> **为什么放 websocket-server 而不是 scrcpy-server**：
> - 下发 `adb-status` 需要设备连接 hub，而 hub 在 websocket-server；scrcpy-server 是无状态的桥接进程（视频/控制），既无设备连接也无 hub，无法直接下发。
> - 若放 scrcpy-server，还需 HTTP 回调 websocket-server 触发下发，凭空多一次跨进程调用。
> - websocket-server 同样可从仓库根启动、`scrcpy.NewADB(".")` 拿到 adb 封装，扫描 + 下发同进程内最内聚。
>
> 结论：扫描与下发都放 websocket-server；scrcpy-server 保持「远程控制会话桥接」单一职责不变。

### 5.2 新增 com 命令 `adb-status`

在 `internal/transport/websocket/commands.go` 增加下行命令（服务端 → 设备端）：

```json
{"type":"com","command":"adb-status","requestId":"...","data":{"connected":false,"ip":"..."}}
```

- 这是服务端主动下发给设备的新命令，设备端 App 需处理。
- 遵循现有 com 协议信封（`type`/`command`/`requestId`/`data`）。

### 5.3 心跳上报扩展：App 附带 IP

设备 `heartbeat` 消息增加可选字段 `ip`。后端 `handleDevice` 的 heartbeat 分支解析并更新「deviceId → IP」内存映射。

### 5.4 mDNS 自动发现 + 自动 connect（阶段 2 内容，可后置）

- 启用 `ADB_MDNS_AUTO_CONNECT`（只自动连接已配对设备）或周期 `adb mdns services`。
- 发现已配对设备后自动 `connect` + `tcpip 5555`。

> 阶段 2 依赖内网 mDNS 可用性，需实测；阶段 1（App 引导 + IP 上报 + 后端 connect）可独立先做。

---

## 6. App 端改动清单（D:\work\wanxiang-android）

### 6.1 心跳附带本机 IP

`ConnectionForegroundService.sendHeartbeat()` 增加读取本机 Wi-Fi IP 并塞进心跳：

```java
msg.put("ip", getLocalIpAddress());
```

`getLocalIpAddress()` 通过 `WifiManager` 或遍历网络接口获取 IPv4（无需特殊权限）。

### 6.2 处理 `adb-status` 下行消息

`DeviceSocketListener.onMessage()` 增加对 `com` + `adb-status` 的处理：

- `connected=false` → 弹系统通知「远程控制已断开，点此恢复」。
- `connected=true` → 撤销该通知。
- 通知点击 → 启动引导 Activity（或直接跳转系统无线调试页）。

### 6.3 一键跳转无线调试页

新增引导逻辑，`startActivity` 跳转系统设置：

```java
// 开发者选项页（Android 11+ 无线调试在其中）
startActivity(new Intent(Settings.ACTION_APPLICATION_DEVELOPMENT_SETTINGS));
```

> 注意：不同 ROM 的「无线调试」入口位置不同，`ACTION_APPLICATION_DEVELOPMENT_SETTINGS` 能跳到开发者选项，用户再手动点「无线调试」。部分 ROM 可能不支持直接深链到无线调试，需引导页给出文字提示。

### 6.4 通知渠道

复用现有通知渠道，或新增一个「远程控制提醒」渠道（`IMPORTANCE_HIGH`，用于掉线提醒）。

### 6.5 权限

- 读 Wi-Fi IP：`ACCESS_WIFI_STATE`（通常已具备或默认授予）。
- 弹通知：`POST_NOTIFICATIONS`（Android 13+，已具备）。

---

## 7. 协议变更汇总（需同步文档）

| 变更 | 方向 | 说明 |
| --- | --- | --- |
| `heartbeat` 增加 `ip` 字段 | 设备 → 服务端 | App 上报本机 IP |
| 新增 com 命令 `adb-status` | 服务端 → 设备 | 下发 ADB 连接状态（connected + ip） |

以上均属 additive 变更，符合 `AGENTS.md` 的兼容要求（只增不改）。

---

## 8. 实施顺序建议

| 阶段 | 内容 | 依赖 | 优先级 |
| --- | --- | --- | --- |
| 1 | App 心跳上报 IP + 后端建立 deviceId↔IP 映射 + 后端扫描判断「业务在线但 ADB 离线」并下发 `adb-status` + App 弹通知/跳转 | 无 | 高（先做，验证半自动闭环） |
| 2 | 后端收到 IP 后自动 `connect`（对已配对设备）+ 自动 `tcpip 5555` | 阶段 1 | 高 |
| 3 | mDNS 自动发现（兜底 IP 变化） | 阶段 1，需实测 mDNS | 中 |
| 4 | 前端设备卡片标注「ADB 已掉线」状态 | 阶段 1 | 低（体验优化） |

---

## 9. 与既有「假完成修复」的关系

本设计与已完成的「假完成状态修复」互补：
- 「假完成修复」解决的是「前端如实反映 ADB 状态」。
- 本设计解决的是「ADB 掉线后，如何引导用户快速恢复」。

两者叠加后，完整体验是：设备掉线 → 前端如实显示「ADB 未连接」+ App 弹通知引导 → 用户开开关 → 后端自动重连。
