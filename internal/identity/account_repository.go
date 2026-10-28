package identity

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 账号自助操作与登录失败计数的 SQL。所有方法接收调用方传入的事务。

// UserTenants 读取该用户的全部成员关系，跨租户。
//
// 调用方必须已经用 db.WithinUser 设好 app.user_id：memberships 是 FORCE RLS，
// 只有 self_membership_read 策略放行 user_id 等于本人的行。这里不再显式过滤
// user_id —— 策略已经是最后一道闸，而 tenants 无 RLS，join 不受影响。
func (r *Repository) UserTenants(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]TenantMembership, error) {
	const q = `SELECT m.tenant_id, t.slug, t.name, m.role, m.status, t.status
	           FROM app.memberships m
	           JOIN app.tenants t ON t.id = m.tenant_id
	           WHERE m.user_id = $1 AND m.deleted_at IS NULL AND t.deleted_at IS NULL
	           ORDER BY m.created_at, t.slug`
	rows, err := tx.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TenantMembership, 0, 4)
	for rows.Next() {
		var m TenantMembership
		if err := rows.Scan(&m.TenantID, &m.Slug, &m.Name, &m.Role, &m.Status, &m.TenantStatus); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateDisplayName 返回是否命中一行（false 表示账号已不存在）。
func (r *Repository) UpdateDisplayName(ctx context.Context, tx pgx.Tx, userID uuid.UUID, name string) (bool, error) {
	const q = `UPDATE app.users
	           SET display_name = $2, updated_by = $1
	           WHERE id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, userID, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdatePassword 以旧哈希为条件更新：并发改密时只有一个能成功。
func (r *Repository) UpdatePassword(ctx context.Context, tx pgx.Tx, userID uuid.UUID, oldHash, newHash string) (bool, error) {
	const q = `UPDATE app.users
	           SET password_hash = $3, updated_by = $1
	           WHERE id = $1 AND password_hash = $2 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, userID, oldHash, newHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RevokeAllRefreshTokens 撤销该账号的全部刷新令牌（跨租户）。
// refresh_tokens 没有 RLS：改密要作用于整个账号，而不只是当前租户。
func (r *Repository) RevokeAllRefreshTokens(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int64, error) {
	const q = `UPDATE app.refresh_tokens
	           SET revoked_at = now()
	           WHERE user_id = $1 AND revoked_at IS NULL`
	tag, err := tx.Exec(ctx, q, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// loginFailure 是失败计数表的一行。
type loginFailure struct {
	FailedCount   int
	FirstFailedAt time.Time
	LockedUntil   *time.Time
}

// LoginFailureForUpdate 锁住该邮箱的计数行；不存在返回 pgx.ErrNoRows。
// 用 FOR UPDATE 而不是 UPSERT：累加、窗口重置与锁定三件事在同一事务里，
// 并发失败不会互相覆盖计数。
func (r *Repository) LoginFailureForUpdate(ctx context.Context, tx pgx.Tx, email string) (*loginFailure, error) {
	const q = `SELECT failed_count, first_failed_at, locked_until
	           FROM app.login_failures
	           WHERE email = $1
	           FOR UPDATE`
	var rec loginFailure
	if err := tx.QueryRow(ctx, q, email).Scan(&rec.FailedCount, &rec.FirstFailedAt, &rec.LockedUntil); err != nil {
		return nil, err
	}
	return &rec, nil
}

// LoginLockedUntil 只读锁定截止时间；未锁定返回零值与 false。
func (r *Repository) LoginLockedUntil(ctx context.Context, tx pgx.Tx, email string) (time.Time, bool, error) {
	const q = `SELECT locked_until FROM app.login_failures
	           WHERE email = $1 AND locked_until IS NOT NULL AND locked_until > now()`
	var until time.Time
	err := tx.QueryRow(ctx, q, email).Scan(&until)
	if IsNoRows(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return until, true, nil
}

func (r *Repository) InsertLoginFailure(ctx context.Context, tx pgx.Tx, email string, count int, first time.Time, lockedUntil *time.Time) error {
	const q = `INSERT INTO app.login_failures (email, failed_count, first_failed_at, locked_until)
	           VALUES ($1, $2, $3, $4)`
	_, err := tx.Exec(ctx, q, email, count, first, lockedUntil)
	return err
}

func (r *Repository) UpdateLoginFailure(ctx context.Context, tx pgx.Tx, email string, count int, first time.Time, lockedUntil *time.Time) error {
	const q = `UPDATE app.login_failures
	           SET failed_count = $2, first_failed_at = $3, locked_until = $4
	           WHERE email = $1`
	_, err := tx.Exec(ctx, q, email, count, first, lockedUntil)
	return err
}

func (r *Repository) ClearLoginFailures(ctx context.Context, tx pgx.Tx, email string) error {
	const q = `DELETE FROM app.login_failures WHERE email = $1`
	_, err := tx.Exec(ctx, q, email)
	return err
}
