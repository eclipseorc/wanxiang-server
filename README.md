# 万象服务端（Wanxiang Server）

Go 实现的 Android 真机云控与自动化管理平台，单 SQLite 存储，纯原生 HTML/CSS/JS 管理后台（无 Node 构建依赖），内置 `tools/adb` 与 `tools/scrcpy-server` 开箱即用。

## 核心能力

- **设备接入与云控**：Android 11+ 免 USB 无线调试自动配对（`adb pair` + 动态端口）、Android 10- 传统 `tcpip 5555` 模式、切网换 IP 自动同步重连。
- **实时控屏与群控**：基于 `scrcpy 5.0.1` + 浏览器 WebCodecs（H.264 60fps），支持单机控屏、多机同步群控与镜像工作台。
- **设备运维**：在线文件管理（浏览/上传/下载）、APK 批量安装/卸载、机型自动配对脚本、分组与细粒度权限控制。
- **RPA 与 AI 智能体**：可视化积木流程编排、50ms 极速无障碍控件树 + SoM 视觉标注、支持接入 OpenAI 兼容多模态大模型执行自然语言任务。

## 服务与端口

| 服务 | 默认端口 | 职责 |
| --- | --- | --- |
| `web-server` | `:9001` | 设备端 HTTP API（以 `deviceId` 换一次性 WS 票据并自动登记） |
| `admin-server` | `:9002` | 管理后台页面（`http://localhost:9002/`）与全部管理 API |
| `websocket-server` | `:9003` | `/ws/device`、`/ws/admin` 实时状态转发、心跳与 `com` 命令协议 |
| `scrcpy-server` | `:9004` | ADB + scrcpy 远程画面与触控代理 |

## 快速启动

要求 **Go 1.23+**，所有命令均在仓库根目录执行：

```powershell
# 1. 初始化配置（设置强 ADMIN_PASSWORD 与 WS_ALLOWED_ORIGINS）
Copy-Item .env.example .env

# 2. 推荐：聚合模式单进程一键启动全部四个服务
go run ./cmd/wanxiang
```

生产/后台运行可使用脚本（支持 `build | single | single-stop | start | stop | restart | status`）：

```powershell
# Windows
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 build
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 single

# Linux / macOS
./scripts/run.sh build && ./scripts/run.sh single
```

> 也可通过 `go run ./cmd/{web,admin,websocket,scrcpy}-server` 分离启动四个独立进程，共享同一份 `.env` 与 SQLite 数据库。

## `.env` 核心配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DB_PATH` | `./data/wanxiang.db` | 本地 SQLite 路径（勿放网络共享盘） |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | `admin` / 必填 | 首次启动创建的管理员账号 |
| `WEB_PORT` / `ADMIN_PORT` / `WS_PORT` / `SCRCPY_PORT` | `:9001`~`:9004` | 各服务监听地址 |
| `WS_ALLOWED_ORIGINS` | 空 | 浏览器管理页精确 Origin，如 `http://localhost:9002` |
| `WEB_DIR` | `./web` | 管理后台静态文件目录 |
| `TLS_CERT` / `TLS_KEY` | 空 | 可选内置 TLS（留空则由反向代理终止 TLS） |

## 延伸文档

- [WebSocket 与 `com` 通信协议](docs/protocol.md)
- [设备 ADB 无线接入指南](docs/device-adb-guide.md)
- [安全基线与上线前必做](docs/security-baseline.md)
- [AI 代理开发手册](AGENTS.md)
