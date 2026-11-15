// Command http-server 初始化 SQLite 和管理员账户，启动管理 API 与静态网页服务。
// 本目录只负责进程启动、HTTP 路由装配和关闭信号处理，业务实现位于 internal 包。
package main
