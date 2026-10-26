// Package outbox 实现事务内 outbox 的存取（ADR-0004）。
//
// 写入必须发生在业务事务内：业务回滚时事件一起消失，业务提交时事件
// 一定已经在表里。投递器再以独立事务领取并发送，失败只影响延迟，不影响业务。
package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
)

// maxErrorRunes 与 outbox_events_last_error_check 的 500 上限保持一致，
// 留一点余量避免截断时正好卡在列约束边界。
const maxErrorRunes = 480

// Event 是投递器领取到的一条待发事件。
type Event struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	AggregateID      uuid.UUID
	AggregateVersion int
	Type             string
	Topic            string
	// Payload 是完整的信封 JSON，原样发送到 Kafka。
	Payload   []byte
	Attempts  int
	CreatedAt time.Time
}

// Store 只做 SQL，不开事务：插入必须复用业务事务，
// 领取、标记则由调用方决定事务边界。
type Store struct{}

func NewStore() *Store { return &Store{} }

// Insert 在给定事务内追加一条事件。信封在这里校验，
// 非法信封（编程错误）直接失败，不会写进 outbox 等待投递器反复重试。
func (s *Store) Insert(ctx context.Context, tx pgx.Tx, topic string, env events.Envelope) error {
	if topic == "" {
		return errors.New("outbox: topic 不能为空")
	}
	raw, err := env.Encode()
	if err != nil {
		return err
	}
	const q = `INSERT INTO app.outbox_events
	    (id, tenant_id, aggregate_id, aggregate_version, event_type, topic, payload)
	 VALUES ($1, $2, $3, $4, $5, $6, $7)`
	if _, err := tx.Exec(ctx, q,
		env.EventID, env.TenantID, env.AggregateID, env.AggregateVersion,
		string(env.Type), topic, string(raw),
	); err != nil {
		return fmt.Errorf("outbox: 写入事件失败: %w", err)
	}
	return nil
}

// Claim 用 SKIP LOCKED 领取至多 limit 条到期事件，并把 available_at
// 推到 lease 之后作为租约：进程崩溃不会让事件永久卡住，租约到期后
// 其他实例可以重新领取。
//
// 调用方必须在发送前提交这次领取，否则锁会一直持有到发送结束。
func (s *Store) Claim(ctx context.Context, tx pgx.Tx, limit int, lease time.Duration) ([]Event, error) {
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("outbox: 领取条数必须在 1..100 之间，当前 %d", limit)
	}
	if lease <= 0 {
		return nil, errors.New("outbox: 租约必须为正")
	}

	const q = `
WITH picked AS (
    SELECT id
    FROM app.outbox_events
    WHERE published_at IS NULL AND available_at <= now()
    ORDER BY available_at, created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE app.outbox_events AS o
SET available_at = now() + ($2::bigint * interval '1 microsecond')
FROM picked
WHERE o.id = picked.id
RETURNING o.id, o.tenant_id, o.aggregate_id, o.aggregate_version,
          o.event_type, o.topic, o.payload, o.attempts, o.created_at`

	rows, err := tx.Query(ctx, q, limit, lease.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("outbox: 领取事件失败: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TenantID, &e.AggregateID, &e.AggregateVersion,
			&e.Type, &e.Topic, &e.Payload, &e.Attempts, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("outbox: 读取事件失败: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: 读取事件失败: %w", err)
	}
	return out, nil
}

// MarkPublished 记录投递成功。保留行本身：outbox 同时充当事件审计日志。
func (s *Store) MarkPublished(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	const q = `UPDATE app.outbox_events
	           SET published_at = now(), last_error = ''
	           WHERE id = $1 AND published_at IS NULL`
	if _, err := tx.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("outbox: 标记已发布失败: %w", err)
	}
	return nil
}

// MarkFailed 记录一次失败并把重试时间推到 retryAt。
// 错误文本落库前截断：第三方错误可能很长，也可能携带不可见字符。
func (s *Store) MarkFailed(ctx context.Context, tx pgx.Tx, id uuid.UUID, cause error, retryAt time.Time) error {
	msg := ""
	if cause != nil {
		msg = truncateRunes(cause.Error(), maxErrorRunes)
	}
	if retryAt.IsZero() {
		retryAt = time.Now()
	}
	const q = `UPDATE app.outbox_events
	           SET attempts = attempts + 1, last_error = $2, available_at = $3
	           WHERE id = $1 AND published_at IS NULL`
	if _, err := tx.Exec(ctx, q, id, msg, retryAt); err != nil {
		return fmt.Errorf("outbox: 标记投递失败失败: %w", err)
	}
	return nil
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
