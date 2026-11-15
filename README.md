# 万象服务端（Go）

Go 实现的设备消息转发器与管理 API。HTTP 管理服务、WebSocket 转发服务独立运行；SQLite 持久化用户、设备、分组、授权关系、会话和审计信息。

管理后台页面：启动 HTTP 服务后打开 `http://localhost:8080/`。页面由纯 HTML/CSS/JavaScript 构成，无需 Node 构建工具。

WebRTC 协议流程与消息样例见 [`docs/webrtc-client-flow.md`](docs/webrtc-client-flow.md)。屏幕采集必须经过 Android 系统 MediaProjection 授权；Go 服务只转发信令，不接收媒体帧。

## 配置与运行

要求 Go 1.23+。项目从仓库根目录读取 `.env`；无需使用 `export`，Windows、macOS 和 Linux 都可以用同一种配置方式。复制 `.env.example` 为 `.env`，再修改密码和地址。`.env` 中已有的值不会覆盖进程环境中显式设置的同名变量。

PowerShell：

```powershell
Copy-Item .env.example .env
go run ./cmd/http-server
```

另开一个终端启动 WebSocket 服务：

```powershell
go run ./cmd/websocket-server
```

Windows 命令��示符也可使用 `copy .env.example .env`。两个服务都必须从项目根目录运行，并使用 `.env` 中同一个本地 SQLite 文件；不要把 SQLite 文件放在网络共享盘。多主机部署需改用服务端数据库和节点间事件/路由机制。

## `.env` 配置项

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DB_PATH` | `./data/wanxiang.db` | SQLite 文件位置；HTTP 和 WebSocket 服务必须相同 |
| `ADMIN_USERNAME` | `admin` | 首次启动创建的管理员用户名 |
| `ADMIN_PASSWORD` | 无 | 必填；请设置强密码 |
| `HTTP_ADDR` | `:8080` | HTTP 管理服务监听地址 |
| `WS_ADDR` | `:8081` | WebSocket 服务监听地址 |
| `WS_ALLOWED_ORIGINS` | 空 | 浏览器管理页面的精确 Origin，逗号分隔；例如 `http://localhost:8080` |
| `WEB_DIR` | `./web` | 管理后台静态文件目录 |

生产环境请使用 HTTPS/WSS，并将浏览器页面的 Origin 准确加入 `WS_ALLOWED_ORIGINS`。设备原生客户端通常不发送 Origin。`.env` 包含密码和私密配置，不要提交到版本控制；仓库提供的 `.env.example` 仅包含示例值。

## API

登录：`POST /api/admin/login`，JSON `{"username":"admin","password":"..."}`。管理员 API 使用 `Authorization: Bearer <token>`。

- `POST /api/admin/users`：创建普通用户 `{"username":"...","password":"至少12字符"}`
- `GET /api/admin/groups`：列出设备组
- `POST /api/admin/groups`：创建组 `{"name":"运营组"}`
- `POST /api/admin/groups/users`：把用户加入用户组 `{"userId":"...","groupId":"..."}`
- `POST /api/admin/groups/devices`：把设备加入设备组 `{"deviceId":"...","groupId":"..."}`
- `POST /api/admin/group-access`：设置组权限 `{"userGroupId":"...","deviceGroupId":"...","canView":true,"canManage":false}`
- `POST /api/admin/devices`：创建设备 `{"name":"..."}`；响应中的 deviceToken 仅此时返回
- `GET /api/admin/devices`：返回当前用户获授权设备
- `POST /api/admin/devices/disconnect`：`{"deviceId":"..."}`；停用设备并关闭当前连接
- `POST /api/admin/devices/enable`：重新启用设备
- `POST /api/admin/ws-ticket`：获取管理端一次性 WebSocket ticket
- `POST /api/device/ws-ticket`：设备用长期设备令牌作为 Bearer 获取一次性 ticket，JSON `{"deviceId":"..."}`

此版本是单 SQLite 主机、单转发节点、单全局观看会话的开发原型，不是多节点生产集群。上线前还需 TLS、速率限制、��份恢复、审计查询、密钥轮换及部署安全评估。
