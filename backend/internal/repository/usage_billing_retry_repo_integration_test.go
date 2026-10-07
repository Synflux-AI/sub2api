//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func claimUsageBillingRetryByID(t *testing.T, queue service.UsageBillingRetryQueue, id int64) (service.UsageBillingRetryItem, bool) {
	t.Helper()
	items, err := queue.ClaimDueUsageBillingRetries(context.Background(), time.Now().Add(time.Second), 1000, time.Minute)
	require.NoError(t, err)
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return service.UsageBillingRetryItem{}, false
}

func usageBillingRetryRowID(t *testing.T, requestID string, apiKeyID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT id FROM usage_billing_retry_queue WHERE request_id = $1 AND api_key_id = $2
	`, requestID, apiKeyID).Scan(&id))
	return id
}

func TestUsageBillingRetryQueue_Lifecycle(t *testing.T) {
	ctx := context.Background()
	repo := NewUsageBillingRepository(testEntClient(t), integrationDB)
	queue, ok := repo.(service.UsageBillingRetryQueue)
	require.True(t, ok, "usage billing repository must implement the retry queue")

	subscriptionID := int64(42)
	cmd := &service.UsageBillingCommand{
		RequestID:        "retry-lifecycle-" + uuid.NewString(),
		APIKeyID:         time.Now().UnixNano(),
		UserID:           7,
		AccountID:        8,
		SubscriptionID:   &subscriptionID,
		Model:            "claude-sonnet-4",
		InputTokens:      10,
		OutputTokens:     6,
		BalanceCost:      0.000078125,
		APIKeyQuotaCost:  0.000078125,
		AccountQuotaCost: 0.5,
	}
	cmd.Normalize()

	require.NoError(t, queue.EnqueueUsageBillingRetry(ctx, cmd, "context deadline exceeded"))
	// 重复入队保持幂等，只保留一行。
	require.NoError(t, queue.EnqueueUsageBillingRetry(ctx, cmd, "again"))
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_billing_retry_queue WHERE request_id = $1 AND api_key_id = $2
	`, cmd.RequestID, cmd.APIKeyID).Scan(&count))
	require.Equal(t, 1, count)

	id := usageBillingRetryRowID(t, cmd.RequestID, cmd.APIKeyID)
	item, found := claimUsageBillingRetryByID(t, queue, id)
	require.True(t, found)
	require.Equal(t, 0, item.Attempts)
	require.Equal(t, cmd, item.Command, "JSONB 往返必须保留指纹与量化后的金额")

	// 租约期内不能被再次领取。
	_, found = claimUsageBillingRetryByID(t, queue, id)
	require.False(t, found)

	// 失败后重排到数据库的「现在」：本轮开始时刻之前的领取窗口看不到它，下一轮即可领到。
	roundStart := time.Now().Add(-time.Second)
	require.NoError(t, queue.RescheduleUsageBillingRetry(ctx, id, 1, "still locked", false))
	sameRound, err := queue.ClaimDueUsageBillingRetries(ctx, roundStart, 1000, time.Minute)
	require.NoError(t, err)
	for _, it := range sameRound {
		require.NotEqual(t, id, it.ID, "本轮失败的条目不能在同一轮被再次领取")
	}
	item, found = claimUsageBillingRetryByID(t, queue, id)
	require.True(t, found)
	require.Equal(t, 1, item.Attempts)

	require.NoError(t, queue.RescheduleUsageBillingRetry(ctx, id, 2, "gave up", true))
	_, found = claimUsageBillingRetryByID(t, queue, id)
	require.False(t, found, "failed 状态不再自动重放")
	var status, lastError string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, last_error FROM usage_billing_retry_queue WHERE id = $1
	`, id).Scan(&status, &lastError))
	require.Equal(t, "failed", status)
	require.Equal(t, "gave up", lastError)

	require.NoError(t, queue.CompleteUsageBillingRetry(ctx, id))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_billing_retry_queue WHERE id = $1
	`, id).Scan(&count))
	require.Zero(t, count)
}

// 复现 issue #270：users 行被长事务持锁，扣费事务等到超时回滚；
// 命令入队后在锁释放后重放，余额只扣一次，再次重放命中幂等键。
func TestUsageBillingRetryQueue_ReplaysAfterRowLockTimeout(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageBillingRepository(client, integrationDB)
	queue := repo.(service.UsageBillingRetryQueue)

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("usage-billing-retry-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      100,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    "sk-usage-billing-retry-" + uuid.NewString(),
		Name:   "billing-retry",
	})

	cmd := &service.UsageBillingCommand{
		RequestID:   "retry-lock-" + uuid.NewString(),
		APIKeyID:    apiKey.ID,
		UserID:      user.ID,
		BalanceCost: 1.25,
	}
	cmd.Normalize()

	holder, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = holder.ExecContext(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, user.ID)
	require.NoError(t, err)

	applyCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, applyErr := repo.Apply(applyCtx, cmd)
	cancel()
	require.Error(t, applyErr)

	require.NoError(t, queue.EnqueueUsageBillingRetry(ctx, cmd, applyErr.Error()))
	require.NoError(t, holder.Rollback())

	var dedupCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2
	`, cmd.RequestID, cmd.APIKeyID).Scan(&dedupCount))
	require.Zero(t, dedupCount, "超时回滚后幂等键不应残留")

	id := usageBillingRetryRowID(t, cmd.RequestID, cmd.APIKeyID)
	item, found := claimUsageBillingRetryByID(t, queue, id)
	require.True(t, found)

	result, err := repo.Apply(ctx, item.Command)
	require.NoError(t, err)
	require.True(t, result.Applied)

	// 模拟出队前进程崩溃后的再次重放：命中幂等键，不重复扣费。
	result, err = repo.Apply(ctx, item.Command)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.NoError(t, queue.CompleteUsageBillingRetry(ctx, id))

	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance FROM users WHERE id = $1", user.ID).Scan(&balance))
	require.InDelta(t, 98.75, balance, 0.000001)
}
