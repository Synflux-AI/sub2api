package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	usageBillingRetryStatusPending = "pending"
	usageBillingRetryStatusFailed  = "failed"
)

var _ service.UsageBillingRetryQueue = (*usageBillingRepository)(nil)

func (r *usageBillingRepository) EnqueueUsageBillingRetry(ctx context.Context, cmd *service.UsageBillingCommand, cause string) error {
	if cmd == nil {
		return nil
	}
	if r == nil || r.db == nil {
		return fmt.Errorf("usage billing repository db is nil")
	}
	cmd.Normalize()
	if cmd.RequestID == "" {
		return service.ErrUsageBillingRequestIDRequired
	}
	payload, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal usage billing command: %w", err)
	}
	// 同一请求重复入队（例如调用方自身被重试）保持第一条，不重置已累计的重试次数。
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO usage_billing_retry_queue (request_id, api_key_id, user_id, command, last_error)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (request_id, api_key_id) DO NOTHING
	`, cmd.RequestID, cmd.APIKeyID, cmd.UserID, payload, cause)
	return err
}

func (r *usageBillingRepository) ClaimDueUsageBillingRetries(ctx context.Context, dueBefore time.Time, limit int, lease time.Duration) ([]service.UsageBillingRetryItem, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("usage billing repository db is nil")
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		UPDATE usage_billing_retry_queue q
		SET next_attempt_at = NOW() + make_interval(secs => $2),
		    updated_at = NOW()
		FROM (
			SELECT id
			FROM usage_billing_retry_queue
			WHERE status = $3 AND next_attempt_at <= $4
			ORDER BY next_attempt_at, id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		) due
		WHERE q.id = due.id
		RETURNING q.id, q.command, q.attempts
	`, limit, lease.Seconds(), usageBillingRetryStatusPending, dueBefore)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.UsageBillingRetryItem, 0, limit)
	for rows.Next() {
		var (
			item    service.UsageBillingRetryItem
			payload []byte
		)
		if err := rows.Scan(&item.ID, &payload, &item.Attempts); err != nil {
			return nil, err
		}
		cmd := &service.UsageBillingCommand{}
		if err := json.Unmarshal(payload, cmd); err != nil {
			return nil, fmt.Errorf("unmarshal usage billing retry %d: %w", item.ID, err)
		}
		item.Command = cmd
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *usageBillingRepository) CompleteUsageBillingRetry(ctx context.Context, id int64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("usage billing repository db is nil")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM usage_billing_retry_queue WHERE id = $1`, id)
	return err
}

func (r *usageBillingRepository) RescheduleUsageBillingRetry(ctx context.Context, id int64, attempts int, cause string, failed bool) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("usage billing repository db is nil")
	}
	status := usageBillingRetryStatusPending
	if failed {
		status = usageBillingRetryStatusFailed
	}
	// pending 的条目把到期时间重置为「现在」：晚于本轮开始时刻，不会在本轮被再次领取，
	// 下一轮即可重放（不必等租约到期）。
	_, err := r.db.ExecContext(ctx, `
		UPDATE usage_billing_retry_queue
		SET attempts = $2,
		    next_attempt_at = NOW(),
		    last_error = $3,
		    status = $4,
		    updated_at = NOW()
		WHERE id = $1
	`, id, attempts, cause, status)
	return err
}
