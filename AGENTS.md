# AGENTS.md

AI 编码代理操作手册。项目背景与部署说明见 `README.md`，详细安全基线与已知限制见 `docs/security-baseline.md`。

## 构建、运行与验收

- 所有命令必须在**仓库根目录**执行（依赖 `.env` 与 `DB_PATH`）。
- 单进程聚合启动：`go run ./cmd/wanxiang`（同时拉起 9001/9002/9003/9004）。
- 编译产物时直接用 `go build`，**不要执行 `scripts/build.bat`**（含 `pause` 会阻塞终端）。
- 改动 Go 代码后提交前必跑：

```sh
gofmt -l .          # 应无输出
go vet ./...
go test ./...
go build -o bin/wanxiang.exe ./cmd/wanxiang
```

## 代码定位地图

| 路径 | 改动什么时看这里 |
| --- | --- |
| `cmd/wanxiang`, `cmd/*-server` | 进程入口与二进制构建 |
| `internal/app` | 服务装配、生命周期、WebSocket 与自动化回调注入 |
| `internal/logic` | 业务规则、认证/票据、统一 ADB 连接器（`AdbConnector`） |
| `internal/storage/sqlite` | 数据库建表/迁移与全部 SQL |
| `internal/transport/webapi` | 设备端 HTTP 接口（`:9001`，换票 + 登记） |
| `internal/transport/adminapi` | 管理后台 HTTP API、Bearer 认证、AI/RPA/scrcpy 路由（`:9002`） |
| `internal/transport/websocket` | `/ws/device`、`/ws/admin`、心跳/IP 同步、`ADBMonitor`、`com` 协议（`:9003`） |
| `internal/scrcpy` | ADB 命令封装、scrcpy H.264 会话与群控同步（`:9004`） |
| `internal/automation` | RPA 引擎、50ms 无障碍/ADB 控件树、SoM 打标、多模态 AI 智能体 |
| `web/` | 单页管理后台（`index.html`、`scrcpy-control.js`、`modules/`） |

## 不可破坏的硬约束

- **SQLite 并发与迁移**：
  - 入口统一用 `sqlite.OpenReady`（`busy_timeout` 必须早于 `journal_mode=WAL`），保持 `MaxOpenConns(1)`。
  - 只允许增量迁移（`CREATE TABLE IF NOT EXISTS` / `ALTER TABLE ADD COLUMN`），禁止重建已有表。
  - `users.created_at` 是 `TEXT` 列，必须写 `YYYY-MM-DD HH:MM:SS`（`timeNowStamp()`），禁止写 Unix 整数。
  - 单测必须用临时库，**严禁**读写 `data/wanxiang.db`；**严禁**暂存或提交 `.env`、`data/*.db`。
- **认证、权限与 WebSocket**：
  - `/ws/admin` 允许普通用户连接，**严禁**加 `role=admin` 校验；所有设备可见性与操作必须走 `Store.CanAccess`。
  - `SetReadDeadline` 只能在读循环所在 goroutine 调用；读上限保持 `512KB`（用于无障碍 UI 树回传）。
  - 服务启动必须调用 `ResetAllOnline()`，快照在线态以内存 `hub.devices` 为准，防止幽灵在线。
  - `device-control` 直接由 WS 进程写库并广播，不要引入经 HTTP 中转的轮询表。
  - 远程画面统一走 scrcpy，**严禁**重新引入已删除的 WebRTC `watch*` / `offer` / `answer` / `candidate` 协议或表。
- **ADB 连接**：
  - `ADB.Connect` 遇到 `adb devices` 中残留的 `offline` 僵尸连接时，必须先 `disconnect` 再 `connect`。
  - Android 11+ 无线调试走服务端 `adb pair` + 动态端口，不要对无线配对设备强切 `tcpip 5555`。
- **安全红线**：
  - 仅 `logic.InputError` 可回显给客户端；其余内部错误只打日志、返回通用文案。
  - 保留登录/票据限流、`dummyPasswordHash` 防枚举、Origin 先校验再消耗 ticket、`justFiles` 静态白名单。

## 编码与兼容约定

- **对外契约只增不减**：HTTP 路径、JSON 字段、`com` 命令、WS `type` 不改名不删除；导出函数签名优先通过新增变体/Options 扩展。
- **命令注册表**：所有 WebSocket 消息与 `com` 命令统一注册在 `internal/transport/websocket/commands.go`（`deviceRoutes` / `adminRoutes` / `comHandlers`），新增命令直接在此注册，无需改 `server.go`；`commandError` 是常量，构造错误函数保持 `newCommandError`。
- **前端脚本顺序**：admin-server 注入顺序固定为 `ws-framework.js` → `admin-dashboard-bridge.js` → `modules/*.js` → `admin-heartbeat.js`，不可打乱。
- **注释与测试**：Go 文件需带中文注释（解释「为什么」而非复述代码）；复用包返回错误，禁止在包内 `os.Exit`；修 bug 补回归测试；改公开 API 或环境变量时同步更新 `README.md` 与 `.env.example`。
