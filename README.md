# 万象服务端（Go）

Go 实现的设备消息转发器与管理 API 原型：HTTP 管理服务、WebSocket 转发服务独立运行；SQLite 持久化用户、设备、分组、权限、会话和审计信息。

## 当前范围

- 管理员登录，服务端签发有时效的 bearer token；数据库只保存 token 哈希，HTTP 和 WebSocket 进程共享 SQLite 会话。
- 管理 API 创建设备/用户/设备组，分配用户与设备组；授权由服务端校验。
- 设备通过长期 WebSocket 上线、发送心跳/状态；在线和状态事件通过管理 WebSocket 实时推送。
- 管理员可请求断开设备（同时暂停其重连），再显式启用。
- 单个全局观看会话；交换 WebRTC `offer`、`answer`、`candidate` 信令。服务端不接收或录制媒体内容。
- Android 端收到 `watch-request` 后必须遵循系统 MediaProjection 用户授权；拒绝时回 `watch-denied`。本项目不提供隐蔽采集或绕过系统授权能力。

## 运行

要求 Go 1.23+。HTTP 与 WebSocket 进程必须使用同一台主机上的同一个 SQLite 文件；不要把 SQLite 文件放共享网络盘。示例默认单机部署。

```sh
go mod tidy
export DB_PATH=./data/wanxiang.db
export ADMIN_USERNAME=admin
export ADMIN_PASSWORD='change-this-before-use'
export HTTP_ADDR=:8080
export WS_ADDR=:8081
go run ./cmd/http-server
```

另开终端启动转发服务（使用相同 `DB_PATH`）：

```sh
export DB_PATH=./data/wanxiang.db
export WS_ADDR=:8081
go run ./cmd/websocket-server
```

首次 HTTP 启动会创建管理员账号。生产环境必须设置强密码、使用 TLS/WSS、限制 WebSocket Origin，并在反向代理正确启用 WebSocket upgrade。HTTP 与 WS 两个进程可以独立部署，但 SQLite 文件要求两者在同一主机；多主机/高可用时应迁移到服务端数据库，并增加跨节点路由/事件总线。

## API 摘要

登录：`POST /api/admin/login`，JSON `{"username":"admin","password":"..."}`。后续管理员 API 使用 `Authorization: Bearer <token>`。

- `POST /api/admin/users`：创建普通用户 `{"username":"...","password":"..."}`
- `POST /api/admin/groups`：创建设备组 `{"name":"..."}`
- `POST /api/admin/groups/users`：分配用户到用户组 `{"userId":"...","groupId":"..."}`
- `POST /api/admin/groups/devices`：把设备分配到设备组 `{"deviceId":"...","groupId":"..."}`
- `POST /api/admin/devices`：创建设备 `{"name":"..."}`；响应只返回一次设备 ID 和设备令牌
- `GET /api/admin/devices`：列出当前用户有权查看的设备
- `POST /api/admin/devices/disconnect`：`{"deviceId":"..."}`，暂停设备并关闭当前连接
- `POST /api/admin/devices/enable`：重新启用设备
- `POST /api/admin/ws-ticket`：获取短时、单次使用的管理 WebSocket ticket
- `POST /api/device/ws-ticket`：设备使用 `Authorization: Bearer <device-token>` 获取短时 ticket，JSON `{"deviceId":"..."}`
- `GET /api/health`

WebSocket：`/ws/device?ticket=...`、`/ws/admin?ticket=...`。Ticket 短时有效且单次消费；不要将长期 bearer token 放在 URL 中。

管理消息：`{"type":"watch","deviceId":"..."}`、`{"type":"watch-stop"}`。设备端收到 `watch-request` 后经系统授权再启动 MediaProjection，并发送 WebRTC 信令消���。

协议消息至少包含 `type`。设备心跳示例：`{"type":"heartbeat","battery":82,"rttMs":34}`。设备端应通过单调时钟测量 RTT，服务端回 `heartbeat-ack`。信令消息格式为 `{"type":"offer|answer","sdp":"..."}` 和 `{"type":"candidate","candidate":"...","sdpMid":"0","sdpMLineIndex":0}`。

## 重要限制

这是面向受控自有设备的开发原型，不是生产就绪的企业平台。默认管理员仅用于首次初始化；请妥善保管数据库文件。当前采用单个转发节点和单个全局观看会话。权限模型为管理员全权，普通用户需同时属于授权用户组，而设备需属于对应设备组；初始组间授权由管理员调用 `POST /api/admin/group-access` 创建（JSON `{"userGroupId":"...","deviceGroupId":"...","canView":true,"canManage":false}`）。建议上线前补充 HTTPS、速率限制、审计查看 API、密钥轮换、备份恢复和安全测试。
