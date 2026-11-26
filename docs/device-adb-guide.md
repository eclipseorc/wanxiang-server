# 设备 ADB 无线接入指南

万象远程控屏、文件管理、RPA 与 AI 智能体均依赖局域网 ADB 连接。按 Android 系统版本分为两种模式：

## 1. 无线调试模式（Android 11+，推荐，全程免 USB 线）

1. 手机开启 **开发者选项**，安装万象设备端 APK，配置服务器地址并连接成功。
2. 开启万象设备端的 **无障碍服务**。
3. 在 App 设置页点击一次 **自动配对**：
   - 无障碍自动打开「无线调试 → 使用配对码配对设备」，读取 6 位配对码与端口；
   - 通过 WebSocket 上报 `adb-pair-request` 给服务端；
   - 服务端执行 `adb pair`（将电脑公钥正式写入手机）→ 直连无线调试动态端口 → 自动执行 `pm grant com.wanxiang.client android.permission.WRITE_SECURE_SETTINGS`。
4. **后续断网/换 IP/重启自恢复**：
   - 切 Wi-Fi 换 IP 时，App 重连 WebSocket 后通过本机 `NsdManager` 自动探测当前无线调试动态端口并上报（不受路由器禁用 mDNS 组播影响），服务端自动更新 IP 并直连。
   - 手机重启后，App 凭借已授予的 `WRITE_SECURE_SETTINGS` 在后台静默重开无线调试并上报新端口。

> 注意：无线调试配对后的设备直接连接动态端口即可，不要再强行执行 `adb tcpip 5555`（部分国产 ROM 会将其打回 `unauthorized`）。

## 2. 传统模式（Android 10 及以下 / 无系统无线调试的机型）

1. 手机开启 **开发者选项 → USB 调试**，用数据线连接服务端电脑。
2. 手机弹出「允许 USB 调试吗？」时，务必勾选 **「一律允许使用这台计算机进行调试」** 并确认。
3. 在管理后台点击该设备的 **连接设备 → 传统模式**（或手动执行 `adb tcpip 5555` 后拔掉数据线），服务端自动连上 `<手机IP>:5555`。
4. 后续只要手机不重启、不撤销 USB 调试授权，即使断网或切换 Wi-Fi 更换局域网 IP，服务端也会自动同步新 IP 并重连 `<新IP>:5555`。
5. **限制**：Android 安全模型规定 `tcpip 5555` 在手机重启后会失效，重启后需重新插一次 USB 线切回 `5555`。

## 3. 常见排查

- **插 USB 线后 `adb devices` 无设备**（常见于 LG 等机型）：下拉通知栏将 USB 用途从「仅充电 / 传输文件」切到 **传输照片（PTP）** 或 **MIDI**；先拔线再开关一次「USB 调试」后重插；优先插电脑主板后置 USB 2.0 口。
- **提示 `unauthorized`（未授权）**：电脑的 `~/.android/adbkey` 公钥尚未写入手机。Android 11+ 重新走一次服务端无线配对即可；Android 10 及以下插一次 USB 线勾选「一律允许」。
- **断网重连后卡 `offline`**：服务端 `ADB.Connect` 已自动先 `adb disconnect` 清理僵尸连接再重连；如手动排查可执行 `adb disconnect <ip:port> && adb connect <ip:port>`。
