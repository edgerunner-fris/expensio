// notification 负责站内通知与邮件外发：消费费用/账单事件，按租户成员分发。
// 监听 :8085。通知不是业务事实来源；邮件走发件箱 + 后台 worker，绝不在业务事务里发信。
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/notification"
	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:           "notification",
		DefaultAddr:       ":8085",
		NeedsDB:           true,
		NeedsJWT:          true,
		NeedsKafka:        true,
		NeedsNotification: true,
	}, setup))
}

// setup 装配收件箱 API、两个事件消费者与邮件 worker。
//
// 内部令牌、identity 地址和 SMTP 参数在配置校验阶段已经检查过；
// Kafka 与 identity 的暂时不可用不会阻止启动：消费者会重连，事件留在主题里。
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
		return fmt.Errorf("notification: 初始化令牌校验器失败: %w", err)
	}
	handler := notification.NewHandler(notification.NewService(database))
	handler.EnableInternalAPI(cfg.Notify.InternalToken)
	notification.RegisterRoutes(app.Server.Engine(), handler, issuer)

	contacts, err := notification.NewContactsClient(cfg.Notify.IdentityURL, cfg.Notify.InternalToken, 3*time.Second)
	if err != nil {
		return err
	}

	// DLQ 与消费共用同一个生产者：两者对 acks=all 的要求一致。
	sec := kafka.SecurityFromConfig(cfg.Kafka)
	producer, err := kafka.NewProducer(cfg.Kafka.Brokers, cfg.Service, sec)
	if err != nil {
		return fmt.Errorf("notification: 初始化 Kafka 生产者失败: %w", err)
	}
	app.OnStop(producer.Close)

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 3*time.Second)
	if err := producer.Ping(pingCtx); err != nil {
		app.Logger.Warn("kafka unavailable at startup; notification consumers will retry",
			zap.Error(err))
	}
	cancelPing()

	consumerOpts := notification.ConsumerOptions{
		Database:     database,
		Contacts:     contacts,
		Brokers:      cfg.Kafka.Brokers,
		Publisher:    producer,
		Logger:       app.Logger,
		ClientID:     cfg.Service,
		EmailEnabled: cfg.Notify.EmailEnabled,
		Security:     sec,
	}
	expenseConsumer, err := notification.NewExpenseConsumer(consumerOpts)
	if err != nil {
		return fmt.Errorf("notification: 初始化费用事件消费者失败: %w", err)
	}
	billingConsumer, err := notification.NewBillingConsumer(consumerOpts)
	if err != nil {
		return fmt.Errorf("notification: 初始化账单事件消费者失败: %w", err)
	}
	// OnStop 逆序执行：停循环 → 关消费者 → 关生产者 → 关连接池。
	app.OnStop(expenseConsumer.Close)
	app.OnStop(billingConsumer.Close)

	// 所有可能失败的构造都放在后台循环启动之前：启动期返回错误时
	// 还没有 goroutine 需要取消，避免上下文泄漏（go vet 的 lostcancel）。
	var worker *notification.EmailWorker
	if cfg.Notify.EmailEnabled {
		mailer, err := notification.NewSMTPMailer(notification.MailerOptions{
			Addr:       cfg.Notify.SMTPAddr,
			From:       cfg.Notify.SMTPFrom,
			Username:   cfg.Notify.SMTPUsername,
			Password:   cfg.Notify.SMTPPassword,
			RequireTLS: cfg.Notify.SMTPUseTLS,
			Timeout:    10 * time.Second,
		})
		if err != nil {
			return err
		}
		worker, err = notification.NewEmailWorker(notification.EmailWorkerOptions{
			Database: database,
			Mailer:   mailer,
			Logger:   app.Logger,
			Interval: 2 * time.Second,
			Batch:    20,
		})
		if err != nil {
			return err
		}
	} else {
		app.Logger.Info("email delivery disabled; notifications remain in-app only")
	}

	bgCtx, stopBackground := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	runConsumer := func(c *notification.Consumer, loop string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Run(bgCtx); err != nil && bgCtx.Err() == nil {
				app.Logger.Error("notification consumer stopped with error",
					zap.String("loop", loop), zap.Error(err))
			}
		}()
	}
	runConsumer(expenseConsumer, "expense")
	runConsumer(billingConsumer, "billing")

	if worker != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker.Run(bgCtx)
		}()
	}

	app.OnStop(func() {
		stopBackground()
		wg.Wait()
	})

	return nil
}
