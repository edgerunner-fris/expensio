// report 负责报表与读模型：消费领域事件，维护按租户划分的汇总表。
// 监听 :8084。读模型可以重建，不作为任何业务事实的来源。
package main

import (
	"os"

	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:     "report",
		DefaultAddr: ":8084",
	}, nil))
}
