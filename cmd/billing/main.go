// billing 负责订阅、账单出账、支付回调与发票。
// 监听 :8083。出账与回调都必须在事务内写 outbox，由独立投递器发 Kafka。
package main

import (
	"os"

	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:     "billing",
		DefaultAddr: ":8083",
	}, nil))
}
