package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Repository 只负责 SQL，不做业务判断、不产生 HTTP 语义。
// 所有方法都接收调用方传入的 pgx.Tx：事务边界由 service 决定，
// 仓储层自己开事务会让"一次业务操作一个事务"变得不可控。
type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

func (r *Repository) TenantBySlug(ctx context.Context, tx pgx.Tx, slug string) (*Tenant, error) {
	const q = `SELECT id, slug, name, plan, status
	           FROM app.tenants
	           WHERE slug = $1 AND deleted_at IS NULL`
	var t Tenant
	err := tx.QueryRow(ctx, q, slug).Scan(&t.ID, &t.Slug, &t.Name, &t.Plan, &t.Status)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *Repository) TenantByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Tenant, error) {
	const q = `SELECT id, slug, name, plan, status
	           FROM app.tenants
	           WHERE id = $1 AND deleted_at IS NULL`
	var t Tenant
	err := tx.QueryRow(ctx, q, id).Scan(&t.ID, &t.Slug, &t.Name, &t.Plan, &t.Status)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *Repository) CreateTenant(ctx context.Context, tx pgx.Tx, t *Tenant, actor uuid.UUID) error {
	const q = `INSERT INTO app.tenants (id, slug, name, plan, status, created_by, updated_by)
	           VALUES ($1, $2, $3, $4, $5, $6, $6)`
	_, err := tx.Exec(ctx, q, t.ID, t.Slug, t.Name, t.Plan, t.Status, actor)
	return err
}

func (r *Repository) UserByEmail(ctx context.Context, tx pgx.Tx, email string) (*User, error) {
	const q = `SELECT id, email, password_hash, display_name, status
	           FROM app.users
	           WHERE lower(email) = lower($1) AND deleted_at IS NULL`
	var u User
	err := tx.QueryRow(ctx, q, email).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.Status)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *Repository) UserByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*User, error) {
	const q = `SELECT id, email, password_hash, display_name, status
	           FROM app.users
	           WHERE id = $1 AND deleted_at IS NULL`
	var u User
	err := tx.QueryRow(ctx, q, id).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.Status)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *Repository) CreateUser(ctx context.Context, tx pgx.Tx, u *User) error {
	const q = `INSERT INTO app.users (id, email, password_hash, display_name, status, created_by, updated_by)
	           VALUES ($1, $2, $3, $4, $5, $1, $1)`
	_, err := tx.Exec(ctx, q, u.ID, u.Email, u.PasswordHash, u.DisplayName, u.Status)
	return err
}

func (r *Repository) CreateMembership(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, role, status string, actor uuid.UUID) error {
	const q = `INSERT INTO app.memberships (tenant_id, user_id, role, status, created_by, updated_by)
	           VALUES ($1, $2, $3, $4, $5, $5)`
	_, err := tx.Exec(ctx, q, tenantID, userID, role, status, actor)
	return err
}

// Membership 必须在 WithinTenant 事务里调用：tenant_id 由 RLS 过滤。
func (r *Repository) Membership(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (*Membership, error) {
	const q = `SELECT user_id, role, status
	           FROM app.memberships
	           WHERE user_id = $1 AND deleted_at IS NULL`
	var m Membership
	err := tx.QueryRow(ctx, q, userID).Scan(&m.UserID, &m.Role, &m.Status)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *Repository) ListMembers(ctx context.Context, tx pgx.Tx, limit, offset int) ([]Member, int, error) {
	const countQ = `SELECT count(*) FROM app.memberships WHERE deleted_at IS NULL`
	var total int
	if err := tx.QueryRow(ctx, countQ).Scan(&total); err != nil {
		return nil, 0, err
	}

	const q = `SELECT m.user_id, u.email, u.display_name, m.role, m.status, m.created_at
	           FROM app.memberships m
	           JOIN app.users u ON u.id = m.user_id
	           WHERE m.deleted_at IS NULL
	           ORDER BY m.created_at, m.user_id
	           LIMIT $1 OFFSET $2`
	rows, err := tx.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	members := make([]Member, 0, limit)
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.Status, &m.CreatedAt); err != nil {
			return nil, 0, err
		}
		// 连接时区跟随 postgres 服务器，统一转 UTC 再出库，前端拿到的永远是同一时区。
		m.CreatedAt = m.CreatedAt.UTC()
		members = append(members, m)
	}
	return members, total, rows.Err()
}

// MemberByID 必须在租户事务里调用：RLS 会把其他租户的成员过滤成"不存在"。
func (r *Repository) MemberByID(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (*Member, error) {
	const q = `SELECT m.user_id, u.email, u.display_name, m.role, m.status, m.created_at
	           FROM app.memberships m
	           JOIN app.users u ON u.id = m.user_id
	           WHERE m.user_id = $1 AND m.deleted_at IS NULL`
	var m Member
	err := tx.QueryRow(ctx, q, userID).Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.Status, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = m.CreatedAt.UTC()
	return &m, nil
}

// UpdateMemberRole 返回是否命中了目标成员（未命中说明该用户不属于当前租户）。
func (r *Repository) UpdateMemberRole(ctx context.Context, tx pgx.Tx, userID uuid.UUID, role string, actor uuid.UUID) (bool, error) {
	const q = `UPDATE app.memberships
	           SET role = $2, updated_by = $3
	           WHERE user_id = $1 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, userID, role, actor)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Repository) CreateRefreshToken(ctx context.Context, tx pgx.Tx, id, tenantID, userID, familyID uuid.UUID, hash []byte, expiresAt time.Time, meta RequestMeta) error {
	const q = `INSERT INTO app.refresh_tokens
	           (id, tenant_id, user_id, token_hash, family_id, expires_at, user_agent, ip)
	           VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::inet)`
	_, err := tx.Exec(ctx, q, id, tenantID, userID, hash, familyID, expiresAt, meta.UserAgent, meta.IP)
	return err
}

func (r *Repository) RefreshTokenByHash(ctx context.Context, tx pgx.Tx, hash []byte) (*RefreshToken, error) {
	const q = `SELECT id, tenant_id, user_id, family_id, expires_at, revoked_at, replaced_by
	           FROM app.refresh_tokens
	           WHERE token_hash = $1`
	var t RefreshToken
	err := tx.QueryRow(ctx, q, hash).Scan(
		&t.ID, &t.TenantID, &t.UserID, &t.FamilyID, &t.ExpiresAt, &t.RevokedAt, &t.ReplacedBy,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// RotateToken 用一个条件更新实现"只能轮换一次"：
// 两个并发刷新同时到达时，只有一个能把 revoked_at 从 NULL 改掉，
// 另一个拿到 0 行，进而被判定为重放。这比先查后写可靠。
func (r *Repository) RotateToken(ctx context.Context, tx pgx.Tx, oldID, newID uuid.UUID) (bool, error) {
	const q = `UPDATE app.refresh_tokens
	           SET revoked_at = now(), replaced_by = $2
	           WHERE id = $1 AND revoked_at IS NULL`
	tag, err := tx.Exec(ctx, q, oldID, newID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Repository) RevokeFamily(ctx context.Context, tx pgx.Tx, familyID uuid.UUID) error {
	const q = `UPDATE app.refresh_tokens
	           SET revoked_at = now()
	           WHERE family_id = $1 AND revoked_at IS NULL`
	_, err := tx.Exec(ctx, q, familyID)
	return err
}

func (r *Repository) InsertAudit(ctx context.Context, tx pgx.Tx, rec *AuditRecord) error {
	meta, err := json.Marshal(rec.Metadata)
	if err != nil {
		return fmt.Errorf("identity: 序列化审计元数据失败: %w", err)
	}
	const q = `INSERT INTO app.audit_logs
	           (id, tenant_id, actor_user_id, action, target_type, target_id, ip, user_agent, metadata)
	           VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::inet, $8, $9)`
	_, err = tx.Exec(ctx, q,
		rec.ID, rec.TenantID, rec.ActorUserID, rec.Action, rec.TargetType, rec.TargetID,
		rec.Meta.IP, rec.Meta.UserAgent, meta,
	)
	return err
}

// ListAudit 必须在 WithinTenant 事务里调用。
func (r *Repository) ListAudit(ctx context.Context, tx pgx.Tx, limit, offset int) ([]AuditEntry, int, error) {
	const countQ = `SELECT count(*) FROM app.audit_logs`
	var total int
	if err := tx.QueryRow(ctx, countQ).Scan(&total); err != nil {
		return nil, 0, err
	}

	const q = `SELECT id, actor_user_id, action, target_type, target_id, metadata, created_at
	           FROM app.audit_logs
	           ORDER BY created_at DESC, id
	           LIMIT $1 OFFSET $2`
	rows, err := tx.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	entries := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var (
			e    AuditEntry
			meta []byte
		)
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.Action, &e.TargetType, &e.TargetID, &meta, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		if len(meta) > 0 {
			if err := json.Unmarshal(meta, &e.Metadata); err != nil {
				return nil, 0, fmt.Errorf("identity: 解析审计元数据失败: %w", err)
			}
		}
		e.CreatedAt = e.CreatedAt.UTC()
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// IsNoRows 把 pgx 的"没查到"收敛成一处判断，避免各层各自 errors.Is。
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// UniqueConstraint 返回违反的唯一约束名，非唯一冲突时返回空串。
// 先查后插挡不住并发注册：两个请求同时通过存在性检查，仍然会撞上唯一索引，
// 这时必须把数据库错误翻译回同样的业务码，否则前端会看到 500。
func UniqueConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}
