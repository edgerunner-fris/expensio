package identity

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 邀请相关的 SQL。所有带租户语义的方法都必须在 WithinTenant /
// WithinTenantID 事务里调用；唯一例外是 FindInvitationByHash，
// 它走 SECURITY DEFINER 函数，因为接受邀请时还没有租户上下文。

func (r *Repository) MemberByEmail(ctx context.Context, tx pgx.Tx, email string) (*Member, error) {
	const q = `SELECT m.user_id, u.email, u.display_name, m.role, m.status, m.created_at
	           FROM app.memberships m
	           JOIN app.users u ON u.id = m.user_id
	           WHERE lower(u.email) = lower($1)
	             AND m.deleted_at IS NULL AND u.deleted_at IS NULL`
	var m Member
	err := tx.QueryRow(ctx, q, email).Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.Status, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = m.CreatedAt.UTC()
	return &m, nil
}

// RevokePendingInvitationByEmail 让同一邮箱的旧链接立即失效。
// 返回撤销条数只用于日志/测试，不构成业务判断。
func (r *Repository) RevokePendingInvitationByEmail(ctx context.Context, tx pgx.Tx, email string) (int64, error) {
	const q = `UPDATE app.invitations
	           SET revoked_at = now()
	           WHERE lower(email) = lower($1)
	             AND accepted_at IS NULL AND revoked_at IS NULL`
	tag, err := tx.Exec(ctx, q, email)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) InsertInvitation(ctx context.Context, tx pgx.Tx, inv *invitationRecord, tokenHash []byte, invitedBy uuid.UUID) error {
	const q = `INSERT INTO app.invitations
	           (id, tenant_id, email, role, token_hash, invited_by, expires_at)
	           VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := tx.Exec(ctx, q, inv.ID, inv.TenantID, inv.Email, inv.Role, tokenHash, invitedBy, inv.ExpiresAt)
	return err
}

// FindInvitationByHash 是接受邀请前的唯一跨租户读路径。
// 返回的 TenantID 来自数据库，后续写操作必须用它建立租户上下文。
func (r *Repository) FindInvitationByHash(ctx context.Context, tx pgx.Tx, hash []byte) (*invitationRecord, error) {
	const q = `SELECT id, tenant_id, tenant_slug, tenant_name, email, role,
	                  expires_at, accepted_at, revoked_at, created_at
	           FROM app.find_invitation($1)`
	var inv invitationRecord
	err := tx.QueryRow(ctx, q, hash).Scan(
		&inv.ID, &inv.TenantID, &inv.TenantSlug, &inv.TenantName, &inv.Email, &inv.Role,
		&inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	inv.ExpiresAt = inv.ExpiresAt.UTC()
	inv.CreatedAt = inv.CreatedAt.UTC()
	return &inv, nil
}

// ListInvitations 返回尚未接受、也未撤销的邀请。
// 已过期但未撤销的也包含在内：管理员需要看到它并主动撤销，
// 前端按 expires_at 标注"已过期"。
func (r *Repository) ListInvitations(ctx context.Context, tx pgx.Tx, limit, offset int) ([]Invitation, int, error) {
	const countQ = `SELECT count(*) FROM app.invitations
	                WHERE accepted_at IS NULL AND revoked_at IS NULL`
	var total int
	if err := tx.QueryRow(ctx, countQ).Scan(&total); err != nil {
		return nil, 0, err
	}

	const q = `SELECT id, email, role, invited_by, expires_at, created_at
	           FROM app.invitations
	           WHERE accepted_at IS NULL AND revoked_at IS NULL
	           ORDER BY created_at DESC, id
	           LIMIT $1 OFFSET $2`
	rows, err := tx.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Invitation, 0, limit)
	for rows.Next() {
		var inv Invitation
		if err := rows.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.InvitedBy, &inv.ExpiresAt, &inv.CreatedAt); err != nil {
			return nil, 0, err
		}
		inv.ExpiresAt = inv.ExpiresAt.UTC()
		inv.CreatedAt = inv.CreatedAt.UTC()
		items = append(items, inv)
	}
	return items, total, rows.Err()
}

func (r *Repository) InvitationByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Invitation, error) {
	const q = `SELECT id, email, role, invited_by, expires_at, created_at
	           FROM app.invitations
	           WHERE id = $1`
	var inv Invitation
	err := tx.QueryRow(ctx, q, id).Scan(&inv.ID, &inv.Email, &inv.Role, &inv.InvitedBy, &inv.ExpiresAt, &inv.CreatedAt)
	if err != nil {
		return nil, err
	}
	inv.ExpiresAt = inv.ExpiresAt.UTC()
	inv.CreatedAt = inv.CreatedAt.UTC()
	return &inv, nil
}

// RevokeInvitation 只撤销仍在等待中的邀请；已接受或已撤销的返回 false。
func (r *Repository) RevokeInvitation(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	const q = `UPDATE app.invitations
	           SET revoked_at = now()
	           WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`
	tag, err := tx.Exec(ctx, q, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// LockPendingInvitation 用行锁串行化并发接受请求：
// 两个请求同时到达时，只有一个能拿到行，另一个会看到状态已经变化。
func (r *Repository) LockPendingInvitation(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	const q = `SELECT 1 FROM app.invitations
	           WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL
	             AND expires_at > now()
	           FOR UPDATE`
	var one int
	err := tx.QueryRow(ctx, q, id).Scan(&one)
	if IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *Repository) AcceptInvitation(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	const q = `UPDATE app.invitations
	           SET accepted_at = now()
	           WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`
	tag, err := tx.Exec(ctx, q, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ActivateMembership 把预先存在的 invited 成员关系转为 active，并采用邀请里的角色。
func (r *Repository) ActivateMembership(ctx context.Context, tx pgx.Tx, userID uuid.UUID, role string, actor uuid.UUID) (bool, error) {
	const q = `UPDATE app.memberships
	           SET role = $2, status = 'active', updated_by = $3
	           WHERE user_id = $1 AND deleted_at IS NULL AND status = 'invited'`
	tag, err := tx.Exec(ctx, q, userID, role, actor)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
