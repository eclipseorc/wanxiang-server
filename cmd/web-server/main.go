// Command web-server 启动面向安卓设备端的 HTTP API（端口 9001）。
// 设备不登录：以 deviceId 作为凭证换取一次性 WebSocket 连接票据。
//
// 本入口只负责委托给 app.RunWeb 并处理致命错误；服务的完整装配与生命周期
// 逻辑位于 internal/app 包，可被单进程聚合入口 cmd/wanxiang 复用。
package main

import (
	"log"

	"github.com/eclipseorc/wanxiang-server/internal/app"
)

func main() {
	if err := app.RunWeb(); err != nil {
		log.Fatal(err)
	}
}
