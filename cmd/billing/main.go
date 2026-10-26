// billing 负责订阅、账单出账、支付回调与发票。
// 监听 :8083。出账与回调都必须在事务内写 outbox，由独立投递器发 Kafka。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/billing"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/outbox"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:             "billing",
		DefaultAddr:         ":8083",
		NeedsDB:             true,
		NeedsJWT:            true,
		NeedsKafka:          true,
		NeedsBillingWebhook: true,
	}, setup))
}

// setup 装配数据库、令牌校验器、回调验签与 outbox 投递器。
//
// 回调密钥缺失或过短会在启动期失败：没有密钥的 billing 对每个回调都只能
// 返回 500，不如不启动。Kafka 相反——broker 暂时不可用只告警，事件已经在
// outbox 里，进程活着就会继续重试。
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
		return fmt.Errorf("billing: 初始化令牌校验器失败: %w", err)
	}

	svc := billing.NewService(database)
	if err := svc.EnableWebhooks([]byte(cfg.Billing.WebhookSecret), cfg.Billing.WebhookProvider); err != nil {
		return fmt.Errorf("billing: 支付回调配置非法: %w", err)
	}
	billing.RegisterRoutes(app.Server.Engine(), billing.NewHandler(svc), issuer)

	producer, err := kafka.NewProducer(cfg.Kafka.Brokers, cfg.Service)
	if err != nil {
		return fmt.Errorf("billing: 初始化 Kafka 生产者失败: %w", err)
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
		return fmt.Errorf("billing: 初始化 outbox 投递器失败: %w", err)
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
