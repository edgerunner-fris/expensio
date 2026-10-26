// notification 负责邮件等外部通知，消费事件并落发送记录。
// 监听 :8085。外部投递必须有幂等键，重复事件不能导致重复发信。
package main

import (
	"os"

	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:     "notification",
		DefaultAddr: ":8085",
	}, nil))
}
