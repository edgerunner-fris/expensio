// expense 负责报销单据、审批状态机、预算占用与扣减。
// 监听 :8082。预算放在本服务是有意为之：预算占用必须和费用写入同一个事务，
// 跨服务会直接破坏一致性（见 docs/adr 中关于预算归属的记录）。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/expense"
	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/outbox"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:     "expense",
		DefaultAddr: ":8082",
		NeedsDB:     true,
		NeedsJWT:    true,
		NeedsKafka:  true,
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

	issuer, err := cfg.JWT.AuthIssuer()
	if err != nil {
		return fmt.Errorf("expense: 初始化令牌校验器失败: %w", err)
	}

	svc := expense.NewService(database)
	expense.RegisterRoutes(app.Server.Engine(), expense.NewHandler(svc), issuer)

	// 事件投递是后台链路：Kafka 不可用不影响 HTTP 服务启动，
	// 也不参与 readiness。事件已经在 outbox 里，进程活着就会继续重试。
	producer, err := kafka.NewProducer(cfg.Kafka.Brokers, cfg.Service)
	if err != nil {
		return fmt.Errorf("expense: 初始化 Kafka 生产者失败: %w", err)
	}
	app.OnStop(producer.Close)

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 3*time.Second)
	if err := producer.Ping(pingCtx); err != nil {
		app.Logger.Warn("kafka unavailable at startup; outbox delivery will retry",
			zap.Error(err))
	}
	cancelPing()

	dispatcher, err := outbox.NewDispatcher(outbox.DispatcherOptions{
		Database:  database,
		Publisher: producer,
		Logger:    app.Logger,
	})
	if err != nil {
		return fmt.Errorf("expense: 初始化 outbox 投递器失败: %w", err)
	}

	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dispatcher.Run(dispatchCtx)
	}()
	// OnStop 逆序执行：先停投递器（dispatcher → producer → database），
	// 否则投递器可能在连接池关闭后写回发布状态。
	app.OnStop(func() {
		stopDispatch()
		<-dispatchDone
	})

	return nil
}
