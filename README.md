# 万象服务端（Go）

Go 实现的设备消息转发器与管理 API。四个服务共享同一个本地 SQLite：设备端 HTTP API、管理后台 HTTP API、WebSocket 转发、ADB 远程控制。SQLite 持久化用户、设备、分组、授权关系、会话和审计信息。

四个服务既可各自独立进程部署（工程化解耦），也可通过 `cmd/wanxiang` 聚合入口在单个进程内一并启动（goroutine 隔离 + 自动重启），详见「配置与运行」。

管理后台页面：启动 admin-server 后打开 `http://localhost:9002/`。页面由纯 HTML/CSS/JavaScript 构成，无需 Node 构建工具。

远程屏幕控制通过 ADB 无线调试 + scrcpy 实现，见下文「scrcpy 远程控制」一节。

## 架构与端口

| 服务 | 端口（默认） | 面向谁 | 职责 |
| --- | --- | --- | --- |
| `web-server` | `:9001` | 安卓设备端 | 设备 HTTP API：以 deviceId 换一次性 ticket |
| `admin-server` | `:9002` | 管理后台浏览器 | 管理员登录、设备/用户/组/权限 API + 静态页面 |
| `websocket-server` | `:9003` | 设备端 + 管理端 | `/ws/device` 与 `/ws/admin` 实时转发 |
| `scrcpy-server` | `:9004` | 管理后台浏览器 | ADB 无线调试 + scrcpy 远程观看/控制 |

设备不登录：App 安装时本地生成 deviceId，首次调用换票接口即自动登记（可带型号）。管理端仍用管理员账密 + session。设备控制（断开/启用）通过管理端的 WebSocket 连接下发，不再经过 HTTP 服务。

## 配置与运行

要求 Go 1.23+。项目从仓库根目录读取 `.env`；无需使用 `export`，Windows、macOS 和 Linux 都可以用同一种配置方式。复制 `.env.example` 为 `.env`，再修改密码和地址。`.env` 中已有的值不会覆盖进程环境中显式设置的同名变量。

四个服务都可独立运行，各自可单独部署、互不影响；并提供了两种「一条命令拉起全部」的方式：**聚合模式**（单进程，推荐日常使用）与**分离模式**（四进程，隔离最强）。

### 两种运行模式

| 模式 | 命令 | 特点 |
| --- | --- | --- |
| **聚合模式** | `single` / `single-stop` | 单个 `wanxiang` 进程内起四个 goroutine，supervisor 做 panic 隔离 + 指数退避自动重启；一条命令一个进程一份日志 |
| **分离模式** | `start` / `stop` | 四个服务各自独立进程，一个挂了完全不影响其他，但需管理四个进程与四份日志 |

两种模式由 `cmd/wanxiang`（聚合入口）与 `cmd/{web,admin,websocket,scrcpy}-server`（四个单服务入口）分别支撑，可自由切换，读同一份 `.env` 与 SQLite。

### 一键启动（推荐）

**Windows**（PowerShell，或直接双击 `scripts\build.bat` / `start.bat` / `stop.bat`）：

```powershell
Copy-Item .env.example .env
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 build   # 编译到 bin\

# 聚合模式（单进程，推荐）
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 single        # 一个进程启动四个服务
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 status        # 查看运行状态
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 single-stop   # 一键停止

# 分离模式（四进程）
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 start         # 后台启动四个独立进程
powershell -ExecutionPolicy Bypass -File scripts\run.ps1 stop          # 一键停止
```

**Linux / macOS**：

```sh
cp .env.example .env
./scripts/run.sh build     # 编译到 bin/

# 聚合模式（单进程，推荐）
./scripts/run.sh single        # 一个进程启动四个服务
./scripts/run.sh status        # 查看运行状态
./scripts/run.sh single-stop   # 一键停止

# 分离模式（四进程）
./scripts/run.sh start         # 后台启动四个独立进程
./scripts/run.sh stop          # 一键停止
```

`run.sh` / `run.ps1` 都支持 `build | start | stop | restart | status | single | single-stop` 子命令。分离模式日志写入 `logs/<service>.log`、PID 写入 `data/pids/<service>.pid`；聚合模式日志写入 `logs/wanxiang.log`、PID 写入 `data/pids/wanxiang.pid`（均已加入 `.gitignore`）。

### 手动逐个启动（调试用）

五个入口都可用 `go run` 启动，便于单独调试某一个：

```powershell
go run ./cmd/wanxiang          # 聚合入口：一个进程起四个服务（带 panic 隔离与自动重启）
go run ./cmd/web-server        # 设备端 HTTP API（9001）
go run ./cmd/admin-server      # 管理后台（9002）
go run ./cmd/websocket-server  # WebSocket 转发（9003）
go run ./cmd/scrcpy-server     # ADB 远程控制（9004）
```

所有服务都必须从项目根目录运行，并使用 `.env` 中同一个本地 SQLite 文件；不要把 SQLite 文件放在网络共享盘。多主机部署需改用服务端数据库和节点间事件/路由机制。

## scrcpy 远程控制（scrcpy-server，`:9004`）

管理后台通过 ADB 无线调试 + scrcpy 远程观看并操作设备屏幕。设备上无需预装 App：`scrcpy-server` 进程在浏览器发起观看时，通过 `adb push` 把 `tools/scrcpy-server`（Android 端 jar）部署到设备的 `/data/local/tmp/scrcpy-server` 并用 `app_process` 启动，随后经 `adb forward` 建立视频（H.264）与控制两条通道，浏览器用 WebCodecs 解码 H.264 并回传触摸/按键/文本/滚动控制事件。

- **配对/连接**走 admin-server 的 `/api/admin/scrcpy/pair` 与 `/connect`，视频与控制流走 scrcpy-server 的 `/api/scrcpy/ws`。
- **scrcpy-server 版本**：对应上游 Genymobile/scrcpy **5.0.1**（git revision `dc8a55a24c27ae78ae202b5a3288da681d0f141a`）。启动命令硬编码的版本参数 `5.0.1` 必须与 `tools/scrcpy-server` 的 jar 真实版本**精确一致**（含补丁版本号），否则 scrcpy-server 会因版本不匹配直接退出。
- **协议差异（v4 → v5）**：v5 的视频流开头从「codec id + width + height」改为「codec id（4 字节）+ session packet（12 字节，含分辨率）」，帧头 flag 位布局也略有调整。`internal/scrcpy/server.go` 的 `readVideoHeader` 已兼容两种格式，`pumpVideo` 会识别并跳过 v5 的 session packet。另 v5 起 `send_codec_meta` 更名为 `send_stream_meta`。
- **保活**：启动命令带 `keep_active=true`（每 ~4 秒模拟用户活动，纯无线设备也有效）与 `stay_awake=true`（仅插电设备有效），避免设备长时间无操作息屏后 MediaProjection 停止推帧、远程画面黑屏。
- **部署优化**：`deploy` 按文件大小比对决定是否重新 push（设备上已有的同名文件大小一致则跳过）。若更换了不同版本的 jar，需留意大小恰好相同时可能不会触发重推，建议必要时手动 `adb push` 或清理设备上的旧文件。
- **分辨率/码率/帧率**：默认 `MaxSize=1920`、`BitRate=8 Mbps`、`MaxFPS=60`（局域网带宽充足），见 `internal/scrcpy/tunnel.go` 的 `DefaultScrcpyConfig`，需重启 scrcpy-server 进程后生效。
- **依赖**：`tools/adb`（Windows 为 `tools/adb.exe`）与 `tools/scrcpy-server` 均随仓库分发，clone 后即可运行，无需额外安装 adb 或 scrcpy。

## `.env` 配置项

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DB_PATH` | `./data/wanxiang.db` | SQLite 文件位置；四个服务必须相同 |
| `ADMIN_USERNAME` | `admin` | 首次启动创建的管理员用户名 |
| `ADMIN_PASSWORD` | 无 | 必填；请设置强密码 |
| `WEB_PORT` | `:9001` | 设备端 HTTP API 监听地址 |
| `ADMIN_PORT` | `:9002` | 管理后台监听地址 |
| `WS_PORT` | `:9003` | WebSocket 转发监听地址 |
| `SCRCPY_PORT` | `:9004` | ADB 远程控制（scrcpy）监听地址 |
| `WS_ALLOWED_ORIGINS` | 空 | 浏览器管理页面的精确 Origin，逗号分隔；例如 `http://localhost:9002` |
| `WEB_DIR` | `./web` | 管理后台静态文件目录 |
| `TLS_CERT` / `TLS_KEY` | 空 | 可选的内置 TLS 证书与私钥；必须同时设置，否则启动报错。仅 admin-server 与 websocket-server 使用；设备端 web-server 默认明文 HTTP |

生产环境请使用 HTTPS/WSS：可以配置 `TLS_CERT` / `TLS_KEY` 让 admin 与 websocket 进程直接监听 TLS，也可以由反向代理终止 TLS（此时 HSTS 由代理设置）。并将浏览器页面的 Origin 准确加入 `WS_ALLOWED_ORIGINS`，启用 TLS 后该值通常是 `https://<域名>`。设备原生客户端通常不发送 Origin。`.env` 包含密码和私密配置，不要提交到版本控制；仓库提供的 `.env.example` 仅包含示例值。

## 安全说明

- 登录失败按「客户端 IP + 用户名」限速：1 分钟内 5 次失败锁定 5 分钟，返回 `429` 与 `Retry-After`；ticket 端点按 IP 限制为 60 次/分钟。两者都只在单个进程内生效。
- 用户名不存在时服务端仍执行一次 bcrypt 校验，抹平登录耗时差，避免账号枚举。
- 会话有效期 12 小时，WebSocket ticket 有效期 60 秒且只能用一次。过期的会话与票据会在服务启动、登录和签发票据时节流清理；审计日志刻意不自动删除。
- 登录、登出、创建用户/组、分组与权限变更、设备控制都会写入审计日志。登录失败只记内存限速，避免暴力破解被放大成数据库写入。
- 设备以 deviceId 作为唯一凭证（无独立令牌）。deviceId 由 App 本地生成、需足够随机，服务端对换票接口做 IP 限速以抑制枚举。管理后台会展示 deviceId，故不把它视为严格机密。
- 全部响应带 `X-Content-Type-Options`、`X-Frame-Options`、`Referrer-Policy`、CSP 与 `Cache-Control: no-store`；启用内置 TLS 时额外下发 HSTS。静态服务禁止目录列表，且只返回白名单扩展名，避免 `WEB_DIR` 误配暴露 `.env` 等文件。
- WebSocket 精确匹配 `WS_ALLOWED_ORIGINS`；未配置时浏览器管理端会被拒绝，转发服务启动时会打印告警。单条连接有 100 条/秒消息上限，设备 1000 / 管理端 200 的连接数上限，设备上报内容广播前会截断；读到任意业务消息都会刷新空闲超时。
- 内部错误只写服务端日志，HTTP 响应统一返回通用消息；输入校验错误会给出可读提示。

## API

### 管理端（admin-server，`:9002`）

登录：`POST /api/admin/login`，JSON `{"username":"admin","password":"..."}`。管理 API 使用 `Authorization: Bearer <token>`。

- `POST /api/admin/users`：创建普通用户 `{"username":"...","password":"12–72 字节"}`；响应中的 `userId` 可直接用于 `POST /api/admin/groups/users`
- `GET /api/admin/devices`：返回当前用户获授权的设备列表
- `GET /api/admin/groups`：列出设备组
- `POST /api/admin/groups`：创建组 `{"name":"运营组"}`
- `POST /api/admin/groups/users`：把用户加入用户组 `{"userId":"...","groupId":"..."}`
- `POST /api/admin/groups/devices`：把设备加入设备组 `{"deviceId":"...","groupId":"..."}`
- `POST /api/admin/group-access`：设置组权限 `{"userGroupId":"...","deviceGroupId":"...","canView":true,"canManage":false}`
- `POST /api/admin/ws-ticket`：获取管理端一次性 WebSocket ticket

设备控制（断开/启用）不是 HTTP 接口，而是管理端 WebSocket 上的 `device-control` 消息：`{"type":"device-control","deviceId":"...","action":"disconnect"|"enable"}`，需 `can_manage` 权限。

### 设备端（web-server，`:9001`）

- `POST /api/device/ws-ticket`：以 deviceId 换一次性 WebSocket ticket，JSON `{"deviceId":"...","model":"..."}`（model 可选）。首次调用自动登记设备，重复调用幂等。

此版本是单 SQLite 主机、单转发节点、单全局远程控制会话的开发原型，不是多节点生产集群。已内置进程内登录限速，但上线前仍需 TLS/WSS、网关层限速、备份恢复、审计查询、密钥轮换及部署安全评估。项目约定与完整限制清单见 [`AGENTS.md`](AGENTS.md)。
