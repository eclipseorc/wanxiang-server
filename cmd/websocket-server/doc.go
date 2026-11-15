// Command websocket-server 初始化共享数据库和 WebSocket Hub，启动设备/管理端转发服务。
// 进程退出时先停止 HTTP 服务，再等待控制事件轮询协程结束。
package main
