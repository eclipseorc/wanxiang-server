# 管理端 WebSocket 客户端骨架

管理页面的 WebSocket 客户端采用“连接传输层 + 消息总线 + 可插拔业务处理器”的结构，参考 yfyk_panel 前端的连接生命周期与消息分发方式，但继续使用本项目的 `/ws/admin` 一次性 ticket 认证和 `watch` 协议。

## 分层职责

- `web/index.html`：现有管理 UI 和业务流程。仍负责向 HTTP API 申请 ticket、创建 `/ws/admin` 连接、并处理设备列表、状态和 WebRTC watch 消息。
- `web/ws-framework.js`：通用客户端骨架。观察管理端 WebSocket 的创建、状态、JSON 收发；提供消息/命令处理器注册与统一发送 API。不包含设备管理、watch 等业务逻辑。
- `web/admin-heartbeat.js`：心跳插件，通过 `WanxiangWS` 注册 `pong/error` 处理器并发送 `com/ping`，不直接接触 socket 变量。

HTTP 服务在返回首页时先注入框架，再于页面业务脚本之后加载心跳插件。框架只拦截 `/ws/admin`，不会干扰设备连接或其他 WebSocket 路径；也不会覆盖现有 `onmessage`，因此业务消息仍由原管理 UI 接收。

## 新增业务的接入方式

新增业务应成为一个独立的前端插件文件（或 JS 模块），调用 `window.WanxiangWS.registerHandler(...)` 注册普通消息处理器，或调用 `registerCommand(command, handler)` 注册 `com` 子命令。插件发送消息时通过 `send()` 或 `sendCommand()`，不要自行创建平行的管理端 WebSocket，也不要改传输层。每个注册方法返回取消注册函数，组件卸载时应调用它清理处理器。

建议业务处理器只解析协议并调用对应业务模块；设备权限、状态修改和持久化仍由服务端校验。不能把服务端传入的任意文本当作 JavaScript/Lua 执行。未来若要支持 Lua，应在明确的隔离沙箱、权限模型、资源限制和审核机制设计后单独实现；当前框架仅支持可信的静态前端插件。

## 连接和消息流程

1. 管理页面通过 `POST /api/admin/ws-ticket` 获取一次性票据。
2. 页面打开 `/ws/admin?ticket=...` 并接收服务端 `device-list` 快照及后续事件。
3. 框架观察 JSON 消息并广播给已注册插件；页面原有消息处理仍继续执行。
4. 心跳插件每 25 秒发送 `com/ping`，用相同 `requestId` 匹配 `com/pong`，10 秒未响应则显示超时。
5. `watch`、`offer`、`answer`、`candidate` 等现有业务消息仍由管理页处理，框架本身不改变这些协议。

`com/ping` 的示例格式：

```json
{"type":"com","command":"ping","requestId":"ping-1","data":{"sentAt":1731670000000}}
```
