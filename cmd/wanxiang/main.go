// Command wanxiang 是万象后端的单进程聚合入口。
//
// 它在单个进程内同时启动四个服务（web / admin / websocket / scrcpy），
// 由 internal/app.Supervise 编排：每个服务运行在独立 goroutine 中，
// panic 被 recover 隔离，失败后按指数退避自动重启，从而在单进程形态下
// 仍保持「一个服务挂了不拖累其他」的隔离体验。
//
// 若需单独部署某个服务，仍可使用 cmd/web-server、cmd/admin-server、
// cmd/websocket-server、cmd/scrcpy-server 下的独立入口。
package main

import (
	"github.com/eclipseorc/wanxiang-server/internal/app"
)

func main() {
	defer app.SetupLogging("wanxiang")()
	app.Supervise([]app.Service{
		{Name: "web", Run: app.RunWeb},
		{Name: "admin", Run: app.RunAdmin},
		{Name: "websocket", Run: app.RunWebsocket},
		{Name: "scrcpy", Run: app.RunScrcpy},
	})
}
