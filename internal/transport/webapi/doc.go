// Package webapi 提供面向安卓设备端的 HTTP 接口。
// 设备不登录：以 deviceId 作为唯一凭证，凭它换取一次性 WebSocket 连接票据。
// 与 adminapi 的差异在于这里没有会话认证、没有管理端静态页面，只服务设备接入。
package webapi
