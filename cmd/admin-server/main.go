// Command admin-server 初始化 SQLite 和管理员账户，启动管理 API 与静态网页服务（端口 9002）。
//
// 本入口只负责委托给 app.RunAdmin 并处理致命错误；服务的完整装配与生命周期
// 逻辑位于 internal/app 包，可被单进程聚合入口 cmd/wanxiang 复用。
package main

import (
	"log"

	"github.com/eclipseorc/wanxiang-server/internal/app"
)

func main() {
	if err := app.RunAdmin(); err != nil {
		log.Fatal(err)
	}
}
