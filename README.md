# 万象服务端（Go）

Go 实现的设备消息转发器与管理 API。三个独立进程共享同一个本地 SQLite：设备端 HTTP API、管理后台 HTTP API、WebSocket 转发。SQLite 持久化用户、设备、分组、授权关系、会话和审计信息。

管理后台页面：启动 admin-server 后打开 `http://localhost:9002/`。页面由纯 HTML/CSS/JavaScript 构成，无需 Node 构建工具。

WebRTC 协议流程与消息样例见 [`docs/webrtc-client-flow.md`](docs/webrtc-client-flow.md)。屏幕采集必须经过 Android 系统 MediaProjection 授权；Go 服务只转发信令，不接收媒体帧。

## 架构与端口

| 服务 | 端口（默认） | 面向谁 | 职责 |
| --- | --- | --- | --- |
| `web-server` | `:9001` | 安卓设备端 | 设备 HTTP API：以 deviceId 换一次性 ticket |
| `admin-server` | `:9002` | 管理后台浏览器 | 管理员登录、设备/用户/组/权限 API + 静态页面 |
| `websocket-server` | `:9003` | 设备端 + 管理端 | `/ws/device` 与 `/ws/admin` 实时转发 |

设备不登录：App 安装时本地生成 deviceId，首次调用换票接口即自动登记（可带型号）。管理端仍用管理员账密 + session。设备控制（断开/启用）通过管理端的 WebSocket 连接下发，不再经过 HTTP 服务。

## 配置与运行

要求 Go 1.23+。项目从仓库根目录读取 `.env`；无需使用 `export`，Windows、macOS 和 Linux 都可以用同一种配置方式。复制 `.env.example` 为 `.env`，再修改密码和地址。`.env` 中已有的值不会覆盖进程环境中显式设置的同名变量。

PowerShell：

```powershell
Copy-Item .env.example .env
go run ./cmd/web-server        # 设备端 HTTP API（9001）
go run ./cmd/admin-server      # 管理后台（9002）
go run ./cmd/websocket-server  # WebSocket 转发（9003）
```

三个服务都必须从项目根目录运行，并使用 `.env` 中同一个本地 SQLite 文件；不要把 SQLite 文件放在网络共享盘。多主机部署需改用服务端数据库和节点间事件/路由机制。

## `.env` 配置项

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DB_PATH` | `./data/wanxiang.db` | SQLite 文件位置；三个服务必须相同 |
| `ADMIN_USERNAME` | `admin` | 首次启动创建的管理员用户名 |
| `ADMIN_PASSWORD` | 无 | 必填；请设置强密码 |
| `WEB_ADDR` | `:9001` | 设备端 HTTP API 监听地址 |
| `ADMIN_ADDR` | `:9002` | 管理后台监听地址 |
| `WS_ADDR` | `:9003` | WebSocket 转发监听地址 |
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

此版本是单 SQLite 主机、单转发节点、单全局观看会话的开发原型，不是多节点生产集群。已内置进程内登录限速，但上线前仍需 TLS/WSS、网关层限速、备份恢复、审计查询、密钥轮换、TURN 配置及部署安全评估。项目约定与完整限制清单见 [`AGENTS.md`](AGENTS.md)。
