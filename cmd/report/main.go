// report 负责报表与读模型：消费领域事件，维护按租户划分的汇总表。
// 监听 :8084。读模型可以重建，不作为任何业务事实的来源。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/report"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:     "report",
		DefaultAddr: ":8084",
		NeedsDB:     true,
		NeedsKafka:  true,
		NeedsJWT:    true,
		NeedsRedis:  true,
	}, setup))
}

// setup 装配数据库、生产者和报表消费者。
//
// Kafka 不可用时进程照常启动：消费循环会持续重连，HTTP 探针不受影响。
// 这与 expense 的处理一致——"消息链路暂时断了"不应该被误报成"服务挂了"。
func setup(app *bootstrap.App) error {
	cfg := app.Config

	database, err := db.Open(context.Background(), cfg.DB.DSN(), cfg.DB.MaxConns)
	if err != nil {
		return err
	}
	app.OnStop(database.Close)
	app.Health.Add(health.Check("db", database.Ping))

	// Redis 只用于仪表盘缓存；它不在 readiness 里，故障时接口回退到现算。
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	app.OnStop(func() { _ = rdb.Close() })

	rdbCtx, cancelRDB := context.WithTimeout(context.Background(), 2*time.Second)
	if err := rdb.Ping(rdbCtx).Err(); err != nil {
		app.Logger.Warn("redis unavailable at startup; report dashboard cache disabled until it recovers",
			zap.Error(err))
	}
	cancelRDB()

	cache := report.NewDashboardCache(rdb, app.Logger)

	// report 不假设网关校验过令牌：所有端点自己再校验一次。
	issuer, err := cfg.JWT.AuthIssuer()
	if err != nil {
		return fmt.Errorf("report: 初始化令牌校验器失败: %w", err)
	}
	report.RegisterRoutes(app.Server.Engine(),
		report.NewHandler(report.NewServiceWithCache(database, cache)), issuer)

	// DLQ 与业务事件共用同一个生产者：两者对 acks=all 的要求一致。
	sec := kafka.Security{
		TLSCAFile:     cfg.Kafka.TLSCAFile,
		SASLMechanism: cfg.Kafka.SASLMechanism,
		SASLUsername:  cfg.Kafka.SASLUsername,
		SASLPassword:  cfg.Kafka.SASLPassword,
	}
	producer, err := kafka.NewProducer(cfg.Kafka.Brokers, cfg.Service, sec)
	if err != nil {
		return fmt.Errorf("report: 初始化 Kafka 生产者失败: %w", err)
	}
	app.OnStop(producer.Close)

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 3*time.Second)
	if err := producer.Ping(pingCtx); err != nil {
		app.Logger.Warn("kafka unavailable at startup; report consumer will retry",
			zap.Error(err))
	}
	cancelPing()

	consumer, err := report.NewConsumer(report.ConsumerOptions{
		Database:  database,
		Brokers:   cfg.Kafka.Brokers,
		Publisher: producer,
		Logger:    app.Logger,
		ClientID:  cfg.Service,
		Cache:     cache,
		Security:  sec,
	})
	if err != nil {
		return fmt.Errorf("report: 初始化报表消费者失败: %w", err)
	}
	app.OnStop(consumer.Close)

	consumeCtx, stopConsume := context.WithCancel(context.Background())
	consumeDone := make(chan struct{})
	go func() {
		defer close(consumeDone)
		if err := consumer.Run(consumeCtx); err != nil {
			app.Logger.Error("report consumer stopped with error", zap.Error(err))
		}
	}()
	// OnStop 逆序执行：先停消费循环，再关客户端、生产者、连接池。
	app.OnStop(func() {
		stopConsume()
		<-consumeDone
	})

	return nil
}
