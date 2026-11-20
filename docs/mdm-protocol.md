# MDM 应用管理协议说明

本文档定义「应用管理」下七个 MDM 功能在 Web 端、后端、Android 端三端之间协作时使用的
策略 key、HTTP 接口、以及 WebSocket 消息格式。实现位置：

- 后端策略与接口：`internal/transport/adminapi/mdm.go`
- 后端存储：`internal/storage/sqlite/{db.go,store.go}`
- 后端 WebSocket 下发：`internal/transport/websocket/server.go`
- Android 端落地：`app/src/main/java/com/wanxiang/client/{MdmPolicyManager.java,ConnectionForegroundService.java,BootReceiver.java}`
- Web 端 UI：`web/index.html`（应用管理标签页）

---

## 一、设计原则（两套逻辑并存）

设备按「是否已被 Device Owner 纳管」分流到两条并行的控制路径：

| 路径 | 判定依据 | 安装/卸载方式 | 策略能力 |
|---|---|---|---|
| **adb 传统路径** | 设备 `mdmEnabled == false` | 服务端直接 `adb install` / `adb uninstall` | 策略类功能不可用（adb 无法拦截安装/卸载/挂起） |
| **MDM 自动化路径** | 设备 `mdmEnabled == true`（Device Owner） | WebSocket 下发指令 → 设备端 `PackageInstaller` 静默安装/卸载 | 五类策略经 WebSocket 下发，设备端 `DevicePolicyManager` 落地 |

纳管状态由设备端心跳上报，服务端落库到 `devices.mdm_enabled`，管理端据此分流。

---

## 二、策略 key 定义（三端共用）

策略是「全局统一」的：一张 `mdm_policies` 表存五类策略，下发到所有已纳管设备。
共 6 个 key，前后端与 Android 端 `MdmPolicyManager` 的常量一一对应：

| key | 含义 | value（JSON 结构） | 设备端落地方式 |
|---|---|---|---|
| `install_policy` | 应用安装限制 | `{"mode":"blacklist"\|"whitelist","packages":["com.a","com.b"]}` | 白名单：`addUserRestriction(DISALLOW_INSTALL_APPS)` 全局禁装；黑名单：`setPackagesSuspended` 挂起黑名单内应用 |
| `uninstall_blocked` | 防卸载应用 | `{"packages":["com.a"]}` | `setUninstallBlocked(pkg, true)` |
| `run_blacklist` | 应用运行黑名单 | `{"packages":["com.a"]}` | `setPackagesSuspended` 挂起（禁用） |
| `keep_alive` | 常驻应用 | `{"packages":["com.a"]}` | `setKeepUninstalledPackages`（API 24+） |
| `auto_start` | 开机自启应用 | `{"packages":["com.a"]}` | 持久化到 `SharedPreferences("mdm_config")`，`BootReceiver` 开机拉起 |
| `device_perms` | 设备权限设置 | 见下方「设备权限策略结构」 | 映射到 `DevicePolicyManager` 的 `addUserRestriction` / `setCameraDisabled` / `setOrganizationName` / `setStayOnPluggedIn` 等 |

> 说明：`install_policy` 的 `mode` 缺省为 `blacklist`。黑白名单语义：
> - **白名单** = 全局禁止安装，白名单内的应用由服务端（adb 或 MDM 路径）主动下发安装；
> - **黑名单** = 不禁止安装动作，只把黑名单内「已安装」的应用挂起（图标灰/不可用）。
>
> 「应用运行黑名单」与「安装限制-黑名单」在设备端落地手段相同（`setPackagesSuspended`），
> 区别在于管理语义：前者关注「禁止运行」，后者关注「禁止使用某类应用」。

### 设备权限策略结构（`device_perms`）

```json
{
  "restrictions": {
    "vpn": false,             // 禁止开启 VPN           → DISALLOW_CONFIG_VPN
    "usbDebug": false,        // 禁止 USB 调试/开发者模式 → DISALLOW_USB_FILE_TRANSFER + 关闭 ADB
    "screenCapture": false,   // 禁止截屏              → DISALLOW_SCREEN_CAPTURE
    "factoryReset": false,    // 禁止恢复出厂设置        → DISALLOW_FACTORY_RESET
    "airplaneMode": false,    // 禁止开启飞行模式        → DISALLOW_AIRPLANE_MODE
    "wifi": false,            // 禁止启用 WiFi          → DISALLOW_CONFIG_WIFI
    "hotspot": false,         // 禁止启用热点           → DISALLOW_CONFIG_TETHERING
    "bluetooth": false,       // 禁止启用蓝牙           → DISALLOW_CONFIG_BLUETOOTH
    "usbFileTransfer": false  // 禁止 USB 传输文件       → DISALLOW_USB_FILE_TRANSFER
  },
  "cameraDisabled": false,    // 禁止启用摄像头          → setCameraDisabled
  "mobileNetwork": "none",    // 移动网络限制："none"|"disableOn"|"disableOff"
  "screenAlwaysOn": false,    // 屏幕常亮               → setStayOnPluggedIn
  "orgName": "",              // 锁屏显示单位名称         → setOrganizationName
  "wifiWhitelist": []         // WIFI 白名单（SSID 列表，当前 Android 无标准白名单 API，仅记录）
}
```

> ⚠️ 暂不支持的三项（无可靠 `DevicePolicyManager` 标准 API，前端仅占位展示，不参与下发）：
> 手机号脱敏、三方通话、通话自动录音。

---

## 三、HTTP 接口（管理端 adminapi，9002）

除 APK 下载端点外，其余均需管理员 session（`Authorization: Bearer <token>`）。

### 3.1 策略 CRUD

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/admin/mdm/policies` | 返回全部策略 `{"policies":[{"key":"...","value":"...","updatedAt":...}]}` |
| `POST` | `/api/admin/mdm/policies` | 写入/更新一条策略，body `{"key":"...","value":"<JSON字符串>"}` |
| `DELETE` | `/api/admin/mdm/policies/{key}` | 删除一条策略 |

校验规则：`key` 必须是上表五个之一（否则 `400 unknown policy key`）；`value` 必须是合法
JSON（否则 `400 value must be valid JSON`）。空字符串 `value` 视为合法（表示清空策略）。

### 3.2 远程安装 / 卸载（自动分流）

| 方法 | 路径 | body | 返回 |
|---|---|---|---|
| `POST` | `/api/admin/mdm/install` | `{"apkId":"...","deviceIds":["..."] ,"mode":"auto"}` | 见下 |
| `POST` | `/api/admin/mdm/uninstall` | `{"packageName":"...","deviceIds":["..."],"mode":"auto"}` | 见下 |

`mode` 可强制指定 `"adb"`（忽略纳管状态走 adb）或 `"mdm"`，缺省 `"auto"`（按纳管状态分流）。

返回结构（两类设备分别收集）：

```json
{
  "package": "com.example.app",
  "adbResults": [ {"deviceId":"...","ok":true,"error":""} ],
  "mdmDevices": [
    {"deviceId":"...","packageName":"com.example.app","apkId":"...","token":"<一次性token>","downloadUrl":"/api/admin/mdm/apk-download?apkId=...&token=..."}
  ]
}
```

- `adbResults`：非 MDM 设备已同步执行 adb 安装/卸载的结果。
- `mdmDevices`：MDM 设备的「下发指令所需数据」，**不会由后端直接推送**，而是返回给
  前端，由前端经管理端 WebSocket 逐台发 `mdm-device-command` 转发给设备（与现有
  `device-control` 同一中转模式）。

### 3.3 APK 下载端点（设备端无 session）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/admin/mdm/apk-download?apkId=xxx&token=yyy` | 设备端下载 APK；token 一次性、绑定 apkId、10 分钟有效 |

响应为 `application/vnd.android.package-archive` 二进制流。

> ⚠️ 部署注意：`apkId` 对应 `apk/upload` 上传后的内存记录，服务重启后失效，需重新上传。
> 设备端需能访问 admin-server（9002）。若设备与 admin-server 不同网段，需在 App 中单独
> 配置 `admin_host` / `admin_port`（见 4.3）。

---

## 四、WebSocket 消息格式

沿用现有协议：设备端连 `/ws/device`，管理端连 `/ws/admin`，消息为 JSON，用 `type` 字段区分。

### 4.1 设备 → 服务端

| type | 方向 | 字段 | 说明 |
|---|---|---|---|
| `heartbeat` | 上行 | 新增 `mdmEnabled: bool` | 心跳里带上纳管状态，服务端落库 `devices.mdm_enabled` |
| `mdm-pull-policy` | 上行 | 无 | 连接建立后主动请求下发当前策略（兜底） |
| `mdm-command-result` | 上行 | `action` `packageName` `ok` `error` | 安装/卸载执行结果回传，服务端转发给所有管理端 |

### 4.2 服务端 → 设备

| type | 方向 | 字段 | 说明 |
|---|---|---|---|
| `mdm-policy` | 下行 | `policies: {key: value字符串}` | 下发全局策略；设备端逐 key 落地 |
| `mdm-device-command` | 下行 | `action`（`install`/`uninstall`）、`packageName`、安装时附 `apkId` `token` `downloadUrl` | 单台设备安装/卸载指令 |

### 4.3 管理端 → 服务端（转发/触发）

| type | 方向 | 字段 | 说明 |
|---|---|---|---|
| `mdm-device-command` | 管理端 WS 上行 | `deviceId` `action` `packageName` [`apkId` `token` `downloadUrl`] | 管理端把 adminapi 返回的指令数据转发给指定设备（仅管理员） |
| `mdm-policy-push` | 管理端 WS 上行 | 无 | 策略变更后触发：服务端读库并广播最新策略给所有在线 MDM 设备（仅管理员） |

服务端对 `mdm-device-command` 会立即回传一条 `mdm-command-result`（`ok:true, sent:true`
表示已送达设备连接；`device offline` / `send failed` 表示未送达）。

---

## 五、MDM 路径完整时序（远程安装为例）

```
管理员            adminapi(9002)        管理端WS(9003)        设备WS(9003)        设备端App
  │ 上传APK          │                     │                   │                   │
  ├─POST apk/upload─▶│                     │                   │                   │
  │ 选设备+APK        │                     │                   │                   │
  ├─POST mdm/install─▶│                     │                   │                   │
  │                   │ 分流: 查 mdm_enabled│                   │                   │
  │◀─ {mdmDevices:[{downloadUrl,token}]}───┤                   │                   │
  │                   │                     │                   │                   │
  ├─WS mdm-device-command──────────────────▶│                   │                   │
  │                   │                     │──mdm-device-command▶│                   │
  │                   │                     │                   │─GET apk-download──▶│(adminapi)
  │                   │                     │                   │◀───APK 二进制─────│
  │                   │                     │                   │  PackageInstaller │
  │                   │                     │◀─mdm-command-result─│  静默安装        │
  │◀─WS mdm-command-result──────────────────┤                   │                   │
```

策略下发的时序更简单：设备连上后发 `mdm-pull-policy` 拉取一次；此后管理员改策略时，
前端调 `POST mdm/policies` 写库，再经 WS 发 `mdm-policy-push`，服务端读库广播
`mdm-policy` 给所有在线 MDM 设备。

---

## 六、约束与已知限制

1. **策略全局统一**：当前不支持按设备/分组差异化配置，一张全局策略表下发所有纳管设备。
2. **Android 端需自行编译验证**：`MdmPolicyManager` 里的 `DevicePolicyManager` 调用依赖
   Device Owner 环境，需在 Android Studio 中编译、并在已纳管真机上验证。
3. **APK 内存态**：`apk/upload` 的 APK 存于 admin-server 进程内存，服务重启后失效。
4. **ADB 传统路径的策略能力为空**：非 MDM 设备不支持安装限制/防卸载/黑名单/常驻/自启
   （这些能力本质是 Device Owner 专有，adb 无法可靠实现）。
5. **Device Owner 静默安装的边界**：依赖设备确为 Device Owner；普通 Device Admin 或
   未纳管设备调用 `PackageInstaller` 会退回用户确认或失败。
