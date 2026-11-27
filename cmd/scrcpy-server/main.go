// Command scrcpy-server 提供远程屏幕查看与控制服务，是 scrcpy 的 WebSocket 代理（端口 9004）。
//
// 职责边界：
//   - 通过 adb（tools/ 下随项目分发）部署、启动设备上的 scrcpy-server。
//   - 在浏览器与设备之间桥接 H.264 视频流与控制事件（一次 WS 连接 = 一次观看）。
//
// 设备配对与连接（adb pair/connect）由 admin-server 负责，本服务只处理观看会话。
//
// 本入口只负责委托给 app.RunScrcpy 并处理致命错误；服务的完整装配与
// 生命周期逻辑位于 internal/app 包，可被单进程聚合入口 cmd/wanxiang 复用。
package main

import (
	"log"

	"github.com/eclipseorc/wanxiang-server/internal/app"
)

func main() {
	defer app.SetupLogging("scrcpy-server")()
	if err := app.RunScrcpy(); err != nil {
		log.Fatal(err)
	}
}
