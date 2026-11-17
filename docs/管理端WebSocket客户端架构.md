# 管理端 WebSocket 客户端架构

管理页面的 WebSocket 客户端按“连接传输层 + 消息总线 + 业务插件”分层。参考 yfyk_panel 前端的连接生命周期与消息分发方式，但继续使用本项目的 `/ws/admin` 一次性 ticket 认证。

## 分层职责

- `web/index.html`：保留管理 UI、ticket 获取、WebSocket 建连和原有页面业务函数。
- `web/ws-framework.js`：通用传输观察、JSON 收发、消息路由和 `com` 命令注册；不包含设备状态业务。
- `web/admin-dashboard-bridge.js`：把已有页面的词法状态封装为小型 API，允许插件渐进迁移页面业务。
- `web/modules/device-events.js`：消费设备列表及设备状态事件。
- `web/admin-heartbeat.js`：心跳插件，负责 `com/ping`、`com/pong` 和响应超时。

HTTP 服务在首页响应中按顺序注入框架、bridge、业务模块和心跳插件。框架只观察 `/ws/admin`，不会干扰设备连接或其他 WebSocket 路径；也不会覆盖原连接的 `onmessage`。bridge 将入站消息送到插件：插件返回 `true` 表示负责处理该类消息，未消费的消息继续走旧处理器。这种方式支持按消息类别逐步拆分，而不是一次性重写整页。

## 新增业务插件

新增业务时，在 `web/modules/` 下增加独立脚本，使用 `WanxiangWS.registerHandler(name, matcher, handler)` 注册普通消息处理，或 `registerCommand(command, handler)` 注册 `com` 子命令。处理器返回 `true` 表示该消息已消费；返回 `false` 或不返回则继续交给旧处理器。

插件发送消息使用 `WanxiangWS.send()` 或 `WanxiangWS.sendCommand()`，不要自行创建另一条管理端 WebSocket。注册函数会返回注销回调，组件卸载或模块停用时应执行清理。

服务端仍负责身份认证、权限检查、状态变更和持久化。不得把服务端传入的任意文本当成 JavaScript 或 Lua 执行。未来若支持 Lua，应另行设计受限沙箱、权限、资源上限和插件审核；当前插件机制只是可信前端代码模块。

## 连接和消息流程

1. 管理页面通过 `POST /api/admin/ws-ticket` 获取一次性票据。
2. 页面打开 `/ws/admin?ticket=...`，接收设备快照和后续事件。
3. bridge 将消息先交给插件；插件处理后返回已消费标记，未迁移消息由旧处理器兜底。
4. 心跳插件每 25 秒发送一次 `com/ping`，通过相同 `requestId` 匹配 `com/pong`；10 秒未响应显示超时。
5. 新增业务以插件扩展，稳定后再考虑从 `index.html` 删除对应的旧逻辑。

示例：

```json
{"type":"com","command":"ping","requestId":"ping-1","data":{"sentAt":1731670000000}}
```
