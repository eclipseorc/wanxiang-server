// Package logic 封装认证、用户与设备创建、临时票据签发等应用业务规则。
// 该包通过 sqlite.Store 访问持久化层，不直接处理 HTTP 或 WebSocket 协议。
package logic
