# 万象服务端（Go）

Go 实现的设备消息转发器与管理 API。HTTP 管理服务、WebSocket 转发服务独立运行；SQLite 持久化用户、设备、分组、授权关系、会话和审计信息。

管理后台页面：启动 HTTP 服务后打开 `http://<server>:8080/`。页面由纯 HTML/CSS/JavaScript 构成，无需 Node 构建工具。

WebRTC 协议流程与消息样例见 [`docs/webrtc-client-flow.md`](docs/webrtc-client-flow.md)，包括设备授权、offer/answer、trickle ICE、会话清理、TURN 配置与测试步骤。屏幕采集必须经过 Android 系统 MediaProjection 授权；Go 服务只转发信令，不接收媒体帧。

## 运行

要求 Go 1.23+。两个进程要在同一主机使用同一个 SQLite 文件；不要通过网络共享盘共享 SQLite 文件。多主机部署需要替换为服务端数据库，并实现节点间事件/连接路由。

```sh
go mod tidy
export DB_PATH=./data/wanxiang.db
export ADMIN_USERNAME=admin
export ADMIN_PASSWORD='change-this-to-a-long-unique-password'
export HTTP_ADDR=:8080
export WS_ADDR=:8081
go run ./cmd/http-server
```

另开终端启动 WebSocket 转发进程，`DB_PATH` 必须一致：

```sh
export DB_PATH=./data/wanxiang.db
export WS_ADDR=:8081
export WS_ALLOWED_ORIGINS=http://localhost:8080
# 正式部署使用 HTTPS/WSS，并设置实际管理页面 Origin
 go run ./cmd/websocket-server
```

WebSocket Origin 必须与 `WS_ALLOWED_ORIGINS` 中某个值完全匹配；无 Origin 的原生客户端可连接。生产环境必须通过 TLS/WSS 暴露服务。

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

此版本是单 SQLite 主机、单转发节点、单全局观看会话的开发原型，不是多节点生产集群。上线前还需 TLS、速率限制、备份恢复、审计查询、密钥轮换及部署安全评估。
