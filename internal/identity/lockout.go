package identity

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/logger"
)

// 登录失败的窗口与锁定参数。做成常量而不是配置：这是安全默认值，
// 需要调整时应该伴随一次有意识的代码变更，而不是环境变量里的手滑。
const (
	maxLoginFailures   = 5
	loginFailureWindow = 15 * time.Minute
	loginLockDuration  = 15 * time.Minute
)

// errTooManyAttempts 与凭证错误分开：429 告诉客户端"现在不要重试"，
// 但它的出现只取决于同一邮箱的失败次数，不暴露账号是否存在。
func errTooManyAttempts() *apperr.Error {
	return apperr.New("auth.too_many_attempts", http.StatusTooManyRequests,
		"登录失败次数过多，请稍后再试")
}

// loginLocked 读取该邮箱当前的锁定状态。
func (s *Service) loginLocked(ctx context.Context, email string) (time.Time, bool, error) {
	var (
		until time.Time
		ok    bool
	)
	err := s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		until, ok, err = s.repo.LoginLockedUntil(ctx, tx, email)
		return err
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return until, ok, nil
}

// recordLoginFailure 累加一次凭证失败，返回本次是否（刚刚或已经）处于锁定。
//
// 独立事务：登录失败会返回错误并回滚业务事务，计数不能跟着回滚。
func (s *Service) recordLoginFailure(ctx context.Context, email string) (bool, error) {
	locked := false
	err := s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now().UTC()
		rec, err := s.repo.LoginFailureForUpdate(ctx, tx, email)
		switch {
		case IsNoRows(err):
			return s.repo.InsertLoginFailure(ctx, tx, email, 1, now, nil)
		case err != nil:
			return err
		}

		// 并发失败已经触发锁定：保持原样，不重复累加。
		if rec.LockedUntil != nil && rec.LockedUntil.After(now) {
			locked = true
			return nil
		}

		count, first := rec.FailedCount+1, rec.FirstFailedAt
		// 锁已过期，或距第一次失败超过窗口：视为新一轮。
		if rec.LockedUntil != nil || now.Sub(first) > loginFailureWindow {
			count, first = 1, now
		}

		var until *time.Time
		if count >= maxLoginFailures {
			t := now.Add(loginLockDuration)
			until = &t
			locked = true
		}
		return s.repo.UpdateLoginFailure(ctx, tx, email, count, first, until)
	})
	if err != nil {
		return false, err
	}
	return locked, nil
}

// loginFailureResult 把一次凭证失败落账，并在达到阈值时返回 429。
// 计数写入失败不阻断登录流程：安全补充手段不应该变成可用性单点。
func (s *Service) loginFailureResult(ctx context.Context, email string) error {
	locked, err := s.recordLoginFailure(ctx, email)
	if err != nil {
		logger.FromContext(ctx).Warn("identity: 记录登录失败次数失败", zap.Error(err))
		return errInvalidCredentials()
	}
	if locked {
		return errTooManyAttempts()
	}
	return errInvalidCredentials()
}
