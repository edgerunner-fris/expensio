// expense 负责报销单据、审批状态机、预算占用与扣减。
// 监听 :8082。预算放在本服务是有意为之：预算占用必须和费用写入同一个事务，
// 跨服务会直接破坏一致性（见 docs/adr 中关于预算归属的记录）。
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/edgerunner-fris/expensio/internal/expense"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:     "expense",
		DefaultAddr: ":8082",
		NeedsDB:     true,
		NeedsJWT:    true,
	}, setup))
}

// setup 装配数据库、令牌校验器与业务服务。任一步失败即启动失败，
// 不会带病提供服务——缺数据库的 expense 对每个请求都只能回 500。
func setup(app *bootstrap.App) error {
	cfg := app.Config

	database, err := db.Open(context.Background(), cfg.DB.DSN(), cfg.DB.MaxConns)
	if err != nil {
		return err
	}
	app.OnStop(database.Close)
	app.Health.Add(health.Check("db", database.Ping))

	issuer, err := auth.NewTokenIssuer(cfg.JWT.Issuer, cfg.JWT.Secret, cfg.JWT.AccessTTL, cfg.JWT.RefreshTTL)
	if err != nil {
		return fmt.Errorf("expense: 初始化令牌校验器失败: %w", err)
	}

	svc := expense.NewService(database)
	expense.RegisterRoutes(app.Server.Engine(), expense.NewHandler(svc), issuer)
	return nil
}
