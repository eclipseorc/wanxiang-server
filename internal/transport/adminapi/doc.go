// Package adminapi 提供管理后台 HTTP API、Bearer 认证与基础安全响应头。
// 认证中间件负责校验 session token，具体接口处理器负责输入校验和业务调用。
// 设备端接口在独立的 webapi 包中，二者不共享认证与路由。
package adminapi
