// identity 负责账号、登录、令牌签发与轮换、租户成员与角色。
// 监听 :8081。token 与密码哈希只在内存和本服务数据库中流转。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/edgerunner-fris/expensio/internal/identity"
	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:                 "identity",
		DefaultAddr:             ":8081",
		NeedsDB:                 true,
		NeedsJWT:                true,
		NeedsJWTSigner:          true,
		NeedsInternalAPI:        true,
		NeedsNotificationClient: true,
	}, setup))
}

// setup 装配 identity 的依赖。返回错误即启动失败，不会带病提供服务。
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
		return fmt.Errorf("identity: 初始化令牌签发器失败: %w", err)
	}

	// 邀请邮件通过 notification 的内部接口投递；identity 不直接连 SMTP。
	notifier, err := identity.NewHTTPNotifier(cfg.Notify.ServiceURL, cfg.Notify.InternalToken, 5*time.Second)
	if err != nil {
		return fmt.Errorf("identity: 初始化邀请邮件客户端失败: %w", err)
	}
	svc, err := identity.NewService(database, issuer, identity.InvitationOptions{
		Notifier: notifier,
		BaseURL:  cfg.Invite.BaseURL,
		TTL:      cfg.Invite.TTL,
	})
	if err != nil {
		return fmt.Errorf("identity: 初始化业务服务失败: %w", err)
	}

	handler := identity.NewHandler(svc)
	handler.EnableInternalAPI(cfg.Notify.InternalToken)
	identity.RegisterRoutes(app.Server.Engine(), handler, issuer)
	return nil
}
