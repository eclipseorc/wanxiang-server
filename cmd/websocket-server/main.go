// Command websocket-server 初始化共享数据库和 WebSocket Hub，启动设备/管理端转发服务（端口 9003）。
//
// 本入口只负责委托给 app.RunWebsocket 并处理致命错误；服务的完整装配与
// 生命周期逻辑位于 internal/app 包，可被单进程聚合入口 cmd/wanxiang 复用。
package main

import (
	"log"

	"github.com/eclipseorc/wanxiang-server/internal/app"
)

func main() {
	if err := app.RunWebsocket(); err != nil {
		log.Fatal(err)
	}
}
