# AGENTS.md

给 AI 编码代理与维护者的项目说明。修改代码前先读 README.md、CONTRIBUTING.md 和本文件。

## 项目是什么

万象服务端：Go 实现的设备消息转发器 + 管理 API。四个独立进程共享同一个本地 SQLite 文件：

- `cmd/web-server`：安卓设备端 HTTP API（`WEB_PORT`，默认 `:9001`），无认证、无静态页。
- `cmd/admin-server`：管理后台 HTTP API + 静态页面（`ADMIN_PORT`，默认 `:9002`）。
- `cmd/websocket-server`：设备端 `/ws/device` 与管理端 `/ws/admin` 转发服务（`WS_PORT`，默认 `:9003`）。
- `cmd/scrcpy-server`：ADB 远程控制代理（`SCRCPY_PORT`，默认 `:9004`），通过 adb 部署并桥接 scrcpy 的 H.264 视频与控制流，不依赖数据库。

远程屏幕控制走 scrcpy（ADB 无线调试），不经 WebSocket 转发进程；不存在 WebRTC 屏幕共享。

## 运行环境与启动

- Go 1.23+。依赖见 `go.mod` / `go.sum`（`go.sum` 必须纳入版本控制）。
- **三个进程都必须从仓库根目录启动**，这样才读得到同一份 `.env` 和同一个 `DB_PATH`。
- 从 `.env.example` 复制出 `.env`，设置强 `ADMIN_PASSWORD`，并把管理页面的精确 Origin 写入 `WS_ALLOWED_ORIGINS`（例如 `http://localhost:9002`）。`.env` 中已有的值不会覆盖进程里显式设置的同名环境变量。

```sh
go run ./cmd/web-server        # 终端 1：设备端 API（9001）
go run ./cmd/admin-server      # 终端 2：管理后台（9002）
go run ./cmd/websocket-server  # 终端 3：WebSocket 转发（9003）
go run ./cmd/scrcpy-server     # 终端 4：ADB 远程控制（9004）
```

## 常用命令

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l $(find . -name '*.go' -not -path './.git/*')   # 应无输出
```

改动 Go 代码后至少跑完以上四条。提交前确认 `.env`、`data/*.db` 没有被暂存。

## 目录结构

| 路径 | 职责 |
| --- | --- |
| `cmd/web-server`, `cmd/admin-server`, `cmd/websocket-server`, `cmd/scrcpy-server` | 依赖装配、服务生命周期、静态页注入、请求日志 |
| `internal/envfile` | 读取 `KEY=value`，已有系统环境变量优先 |
| `internal/logic` | 业务规则：密码、用户、设备登记、会话、一次性票据 |
| `internal/storage/sqlite` | 数据库连接、表结构与迁移、全部 SQL |
| `internal/transport/webapi` | 设备端 HTTP 路由（无认证、换票 + 设备登记） |
| `internal/transport/adminapi` | 管理端 HTTP 路由、Bearer 认证、登录限速、安全响应头、scrcpy 配对/连接接口 |
| `internal/transport/websocket` | 连接中枢、消息转发、`com` 命令协议、设备控制 |
| `internal/scrcpy` | scrcpy 协议编解码、adb 封装、会话与隧道管理 |
| `pkg/logger` | 预留的日志封装，当前未被引用 |
| `web/` | 管理后台页面与可插拔 WebSocket 客户端模块 |
| `docs/` | 协议、客户端架构与中文代码导读 |

## 架构约束（不要轻易假设可以扩展）

- **单个 SQLite 主机、单个转发节点、单个全局 scrcpy 远程控制会话**。`scrcpy` 包对同一设备（serial）只允许一个观看会话。
- 设备控制（断开/启用）是管理端 WebSocket 上的 `device-control` 消息，由转发进程直接写库并广播；**不存在** `control_events` 表与轮询机制，不要重新引入「经 HTTP 服务中转控制指令」的绕路设计。
- `db.SetMaxOpenConns(1)` + WAL + `busy_timeout=5000`，跨进程写入靠 SQLite 锁串行化。不要把 DB 放到网络共享盘。
- 启动入口统一用 `sqlite.OpenReady`（打开 + 建表 + 有限重连重试）。两个进程同时启动一个新库时，第二个进程创建 WAL/SHM 共享文件可能瞬间拿到 `SQLITE_BUSY` / `SQLITE_READONLY`；`busy_timeout` 必须先于 `journal_mode=WAL` 设置，否则对 PRAGMA 不生效。测试代码仍可用 `Open` + `InitSchema`。
- 多节点部署需要服务端数据库、节点间事件/路由机制，以及共享的登录限速存储。

## 认证与协议要点

- 管理员：`POST /api/admin/login` → Bearer session token（**12 小时**），存的是 SHA-256 摘要。
- WebSocket：先经各自 HTTP 服务取一次性 ticket（**60 秒**），再连接 `?ticket=...`；ticket 用后即删，类型 `admin` / `device` 必须与目标路径匹配。
- 设备：**不登录、无独立令牌**。以 deviceId 作为唯一凭证调 `POST /api/device/ws-ticket` 换 ticket，首次调用即登记设备（可带 model）。deviceId 由 App 本地生成。
- 应用层命令统一信封 `{"type":"com","command":"...","requestId":"...","data":{...}}`，白名单分发，未知命令返回 `command:"error"`；它与 WebSocket 控制帧 Ping/Pong 是两套机制。
- 权限全部在服务端判定：admin 拥有全部权限；普通用户需经 `group_users` → `group_access` → `device_groups` 获得 `can_view` / `can_manage`。

## 代码约定

- 所有 Go 文件必须 `gofmt`。注释说明「为什么」，不要复述一行代码；包用途、导出声明、生命周期、非显然的决策要写注释。
- 复用包返回错误给命令入口，不要在包内终止进程。
- **同包内常量与函数不能重名**。`commands.go` 里 `commandError` 是值为 `"error"` 的常量，构造错误响应的函数因此必须叫 `newCommandError`；协议文档与测试都依赖该常量。
- `logic.InputError` 表示可安全回显给客户端的校验错误；其它错误只写服务端日志，返回通用消息，避免泄露 SQL / bcrypt 细节。
- `users.created_at` 是 TEXT 列，写入必须用与 SQLite `CURRENT_TIMESTAMP` 一致的 `YYYY-MM-DD HH:MM:SS` 字符串（`timeNowStamp()`），不能写 Unix 整数。
- 改 API、WebSocket 消息或部署行为时同步更新 `docs/` 和 README.md。

## 安全基线（已实现，改动时不要回退）

- 登录失败限速：按「IP + 用户名」1 分钟内 5 次失败锁定 5 分钟，返回 429 + `Retry-After`；成功即清零。计数表满时直接拒绝新 key，不允许静默放行。
- ticket 端点按 IP 限制 60 次/分钟；设备 1000 / 管理端 200 的连接数上限。
- 用户名不存在时仍执行一次等成本 bcrypt 校验（`dummyPasswordHash`），抹平登录耗时差，防止账号枚举；失败响应文本完全统一。
- `sessions` / `ws_tickets` 过期行在启动、登录、签发票据时节流清理；`audit_logs` 刻意不自动删除。
- 登录、登出、创建用户/组、分组与权限变更、设备控制都写审计；登录失败不写 DB，避免攻击放大写入。
- HTTP 安全头：`X-Content-Type-Options`、`X-Frame-Options`、`Referrer-Policy`、CSP、`Cache-Control: no-store`（全站）；启用内置 TLS 时加 HSTS。页面仍有内联脚本，CSP 保留 `'unsafe-inline'`。
- 静态服务禁止目录列表且只允许白名单扩展名（`justFiles`），即使 `WEB_DIR` 误配也不会暴露 `.env` / `go.mod` / `*.db`。
- WebSocket：Origin 精确白名单（Origin 拒绝时先拦截，不消耗 ticket）、单次 ticket、64KB 读上限、单连接 100 条/秒限速、设备上报内容广播前截断、读到任意业务消息都刷新 `pongWait` 空闲超时、5 秒写超时。
- 可选内置 TLS：`TLS_CERT` / `TLS_KEY` 必须同时设置，两个进程共用，最低 TLS 1.2；留空则由反向代理终止 TLS。
- 客户端 IP 取 `RemoteAddr`，**不信任** `X-Forwarded-For`（除非确认部署了可信反代）。
- 内部错误只写服务端日志；只有 `logic.InputError` 会回显给客户端。`CanAccess` 用两条固定 SQL，不做列名拼接。

## 已知限制 / 上线前必做

1. TLS 可内置启用，但默认仍是明文 `:9001` / `:9002` / `:9003`；由反向代理终止 TLS 时 HSTS 需代理设置。
2. 登录限速与 ticket 限速都是进程内内存态，多实例需共享存储；仍需网关层总体限速与账户锁定策略。
3. ticket 通过 URL query 传递，会经过浏览器历史与反向代理访问日志。代码日志已刻意排除 query，但部署时必须确认代理日志脱敏。
4. 无 Origin 头的非浏览器客户端一律放行（设备原生客户端依赖此行为），CSWSH 风险由单次 ticket 缓解；若收紧需给设备端单独方案。
5. 设备以 deviceId 为唯一凭证、无独立令牌：任何拿到 deviceId 的人都能以该设备身份换票连入。deviceId 需由 App 用强随机生成并足够长，且换票接口有 IP 限速兜底，但这是「标识即凭证」的固有取舍。
6. `broadcastDevice` 同步串行向管理端推送并逐个查库，慢客户端会拖慢该次广播。高并发场景需要每连接发送队列 + writer goroutine。
7. scrcpy 的「同一设备仅一个观看会话」由 `scrcpy.Server.tryAcquire/release` 在内存中维护；进程重启后会重置，属单节点原型取舍。
8. `groups` 表不区分用户组/设备组类型，`group_users` / `device_groups` / `group_access` 也允许任意组 ID 交叉引用；服务端未校验语义正确性，依赖管理端正确配置。
9. 管理员密码只在账号不存在时由 `.env` 创建，改 `.env` 不会轮换已有密码；需要显式轮换流程。
10. 审计日志已覆盖主要管理操作但尚无查询接口；日志不自动清理，需要备份/归档策略。缺少日志轮转。
11. 现有历史库中可能残留写入 Unix 秒字符串的 `users.created_at` 值，属于旧版本遗留，不影响功能。

## 常见坑

- **不要**把「补注释 / 排版」当成无风险改动。历史上一次纯注释重构曾把 `CreateUser(id, ...)` 的 ID 参数删掉，并让 `CreateUser` 返回字符串 `"created"` 冒充 userId，导致「创建用户 → 加入用户组」链路整体失效。
- **普通用户也允许连接 `/ws/admin`**，用于实时查看获授权设备。`hub.admins` 是「管理端通道连接」集合，不是管理员集合；不要给 `adminTicket` 或 `Handler.admin()` 加 `role=admin` 校验，否则会破坏权限模型。所有设备可见性必须走 `Store.CanAccess`。
- 修改 `Store.CreateUser` 签名时，必须同步检查 `internal/logic` 与 `internal/transport/adminapi` 的调用点。
- 修改读超时逻辑时记住：`SetReadDeadline` 只能在读循环所在 goroutine 调用。
- 同包内常量与函数不能重名：`commandError` 是值为 `"error"` 的常量，错误响应构造函数必须叫 `newCommandError`。
- 前端脚本由 admin-server 按 `ws-framework.js` → `admin-dashboard-bridge.js` → `modules/*.js` → `admin-heartbeat.js` 顺序注入；bridge 依赖页面的全局词法绑定，顺序变了插件会安全退出。
- 测试使用临时 SQLite 库；不要指向 `data/wanxiang.db`，也不要提交该文件。


## 维护约定（API 稳定性、兼容性与贡献流程）

- **API 稳定性**：优先保持导出函数/类型的签名不变。需要新增参数时，新增变体函数（如 `FooWithOptions`）或改用 options 结构体，不要直接改既有签名；确需破坏签名时，同步更新所有调用点、测试与文档，并在提交说明中明确标注。
- **向后兼容**：HTTP 路径、JSON 字段名、`com` 命令名与 WebSocket 消息 `type` 都是对外契约，只能新增、不能改名或删除；响应码与错误文案的变化也要写进 README。数据库只做 additive 迁移（`CREATE TABLE IF NOT EXISTS` + `ALTER TABLE ADD COLUMN`），不重建已有表。
- **已移除的 WebRTC 屏幕共享**：`watch` / `watch-stop` / `watch-started` / `watch-ready` / `watch-denied` / `watch-ended` / `watch-stopped` / `offer` / `answer` / `candidate` 等 WebSocket 消息类型，以及 `watch_sessions` 表、`StartWatch` / `EndWatch` / `ResetWatch` 已整体删除，屏幕查看功能由 scrcpy 远程控制取代。**不要重新引入**这些类型或表；如需恢复需重新设计并与现有 scrcpy 方案对齐。
- **贡献流程**：改动前先读本文件；改动后跑 `gofmt -l`、`go build`、`go vet`、`go test` 四条检查；PR 描述需说明行为变化、兼容影响与测试结果；发布前确认 `.env`、`data/*.db` 未被提交。
- **文档维护**：改 API / WebSocket 消息 / 部署行为时，必须同步更新 `README.md`、`docs/` 与本文件；新增配置项要同时写进 `.env.example`。
- **代码注释**：所有 Go 源文件都必须有中文注释，不允许存在通篇零注释的文件。包用途写在 `doc.go`；导出声明、生命周期、并发约束和非显然的决策都要有注释，注释解释「为什么」而不是复述代码。
- **回归测试**：修复 bug 时补一条能复现该 bug 的测试；测试使用临时 SQLite 库，不指向 `data/wanxiang.db`。