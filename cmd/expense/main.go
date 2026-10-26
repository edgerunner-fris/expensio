// expense 负责报销单据、审批状态机、预算占用与扣减。
// 监听 :8082。预算放在本服务是有意为之：预算占用必须和费用写入同一个事务，
// 跨服务会直接破坏一致性（见 docs/adr 中关于预算归属的记录）。
package main

import (
	"os"

	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:     "expense",
		DefaultAddr: ":8082",
	}, nil))
}
