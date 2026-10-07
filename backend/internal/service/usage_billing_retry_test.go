//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// usageBillingRetryQueueStub 同时实现计费仓储与补扣队列，模拟生产里的 usageBillingRepository。
type usageBillingRetryQueueStub struct {
	openAIRecordUsageBillingRepoStub

	enqueueErr  error
	enqueued    []*UsageBillingCommand
	causes      []string
	enqueueCtxs []error

	claimItems  []UsageBillingRetryItem
	claimCalls  int
	completed   []int64
	rescheduled []usageBillingRetryRescheduleCall
}

type usageBillingRetryRescheduleCall struct {
	id       int64
	attempts int
	cause    string
	failed   bool
}

func (s *usageBillingRetryQueueStub) EnqueueUsageBillingRetry(ctx context.Context, cmd *UsageBillingCommand, cause string) error {
	s.enqueueCtxs = append(s.enqueueCtxs, ctx.Err())
	if s.enqueueErr != nil {
		return s.enqueueErr
	}
	s.enqueued = append(s.enqueued, cmd)
	s.causes = append(s.causes, cause)
	return nil
}

func (s *usageBillingRetryQueueStub) ClaimDueUsageBillingRetries(_ context.Context, _ time.Time, limit int, _ time.Duration) ([]UsageBillingRetryItem, error) {
	s.claimCalls++
	items := s.claimItems
	if len(items) > limit {
		items = items[:limit]
	}
	s.claimItems = s.claimItems[len(items):]
	return items, nil
}

func (s *usageBillingRetryQueueStub) CompleteUsageBillingRetry(_ context.Context, id int64) error {
	s.completed = append(s.completed, id)
	return nil
}

func (s *usageBillingRetryQueueStub) RescheduleUsageBillingRetry(_ context.Context, id int64, attempts int, cause string, failed bool) error {
	s.rescheduled = append(s.rescheduled, usageBillingRetryRescheduleCall{
		id: id, attempts: attempts, cause: cause, failed: failed,
	})
	return nil
}

func gatewayBillingRetryInput(requestID string) *RecordUsageInput {
	return &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: requestID,
			Usage:     ClaudeUsage{InputTokens: 10, OutputTokens: 6},
			Model:     "claude-sonnet-4",
			Duration:  time.Second,
		},
		APIKey:  &APIKey{ID: 505},
		User:    &User{ID: 605},
		Account: &Account{ID: 705},
	}
}

func TestGatewayServiceRecordUsage_BillingTimeoutDefersToRetryQueue(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &usageBillingRetryQueueStub{}
	billingRepo.err = context.DeadlineExceeded
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), gatewayBillingRetryInput("gateway_billing_timeout"))

	require.NoError(t, err)
	require.Equal(t, 1, billingRepo.calls)
	require.Len(t, billingRepo.enqueued, 1)
	require.Same(t, billingRepo.lastCmd, billingRepo.enqueued[0])
	require.NotEmpty(t, billingRepo.enqueued[0].RequestFingerprint, "入队命令必须带上原指纹，重放才能命中同一幂等键")
	require.Greater(t, billingRepo.enqueued[0].BalanceCost, 0.0)
	require.Contains(t, billingRepo.causes[0], context.DeadlineExceeded.Error())
	require.NoError(t, billingRepo.enqueueCtxs[0], "入队必须使用新的 ctx，不能沿用已超时的计费 ctx")

	require.Equal(t, 1, usageRepo.calls)
	require.NotNil(t, usageRepo.lastLog)
	require.Greater(t, usageRepo.lastLog.ActualCost, 0.0, "已入补扣队列的请求不能按 0 记账")
	require.Equal(t, billingRepo.enqueued[0].BalanceCost, QuantizeUsageBillingAmount(usageRepo.lastLog.ActualCost))
}

func TestGatewayServiceRecordUsage_BillingErrorFallsBackToZeroWhenEnqueueFails(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingErr := errors.New("billing tx failed")
	billingRepo := &usageBillingRetryQueueStub{enqueueErr: errors.New("db down")}
	billingRepo.err = billingErr
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), gatewayBillingRetryInput("gateway_billing_enqueue_fail"))

	require.ErrorIs(t, err, billingErr)
	require.Empty(t, billingRepo.enqueued)
	require.NotNil(t, usageRepo.lastLog)
	require.Zero(t, usageRepo.lastLog.ActualCost)
}

func TestGatewayServiceRecordUsage_FingerprintConflictIsNotDeferred(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &usageBillingRetryQueueStub{}
	billingRepo.err = ErrUsageBillingRequestConflict
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), gatewayBillingRetryInput("gateway_billing_conflict"))

	require.ErrorIs(t, err, ErrUsageBillingRequestConflict)
	require.Empty(t, billingRepo.enqueued)
	require.Zero(t, usageRepo.lastLog.ActualCost)
}

func TestOpenAIGatewayServiceRecordUsage_BillingTimeoutDefersToRetryQueue(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &usageBillingRetryQueueStub{}
	billingRepo.err = context.DeadlineExceeded
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_billing_timeout",
			Usage:     OpenAIUsage{InputTokens: 20, OutputTokens: 10},
			Model:     "gpt-5.1",
			Duration:  time.Second,
		},
		APIKey:  &APIKey{ID: 1001},
		User:    &User{ID: 2001},
		Account: &Account{ID: 3001},
	})

	require.NoError(t, err)
	require.Len(t, billingRepo.enqueued, 1)
	require.Greater(t, billingRepo.enqueued[0].BalanceCost, 0.0)
	require.NotNil(t, usageRepo.lastLog)
	require.Greater(t, usageRepo.lastLog.ActualCost, 0.0)
}

func newUsageBillingRetryServiceForTest(repo *usageBillingRetryQueueStub) *UsageBillingRetryService {
	svc := NewUsageBillingRetryService(repo, nil, nil, time.Second, 0)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixed }
	return svc
}

func TestUsageBillingRetryService_ReplaySuccessCompletesItem(t *testing.T) {
	repo := &usageBillingRetryQueueStub{}
	cmd := &UsageBillingCommand{RequestID: "r1", APIKeyID: 1, UserID: 2, BalanceCost: 1}
	repo.claimItems = []UsageBillingRetryItem{{ID: 11, Command: cmd, Attempts: 0}}
	svc := newUsageBillingRetryServiceForTest(repo)

	require.Equal(t, 1, svc.runOnce(context.Background()))
	require.Equal(t, 1, repo.calls)
	require.Same(t, cmd, repo.lastCmd)
	require.Equal(t, []int64{11}, repo.completed)
	require.Empty(t, repo.rescheduled)
}

func TestUsageBillingRetryService_AlreadyAppliedCompletesItem(t *testing.T) {
	// 原事务其实已提交（提交应答丢失）：重放命中幂等键返回 Applied=false，直接出队。
	repo := &usageBillingRetryQueueStub{}
	repo.result = &UsageBillingApplyResult{Applied: false}
	repo.claimItems = []UsageBillingRetryItem{{ID: 12, Command: &UsageBillingCommand{RequestID: "r2", APIKeyID: 1}}}
	svc := newUsageBillingRetryServiceForTest(repo)

	svc.runOnce(context.Background())
	require.Equal(t, []int64{12}, repo.completed)
	require.Empty(t, repo.rescheduled)
}

func TestUsageBillingRetryService_RetryableFailureStaysPendingForNextRound(t *testing.T) {
	repo := &usageBillingRetryQueueStub{}
	repo.err = context.DeadlineExceeded
	repo.claimItems = []UsageBillingRetryItem{{ID: 13, Command: &UsageBillingCommand{RequestID: "r3", APIKeyID: 1}, Attempts: DefaultUsageBillingRetryMaxAttempts - 2}}
	svc := newUsageBillingRetryServiceForTest(repo)

	svc.runOnce(context.Background())
	require.Empty(t, repo.completed)
	require.Len(t, repo.rescheduled, 1)
	call := repo.rescheduled[0]
	require.Equal(t, int64(13), call.id)
	require.Equal(t, DefaultUsageBillingRetryMaxAttempts-1, call.attempts)
	require.False(t, call.failed, "未用尽重试次数时保持 pending，等下一轮")
	require.Contains(t, call.cause, context.DeadlineExceeded.Error())
}

func TestUsageBillingRetryService_DrainsAllDueItemsInOneRound(t *testing.T) {
	repo := &usageBillingRetryQueueStub{}
	total := usageBillingRetryBatchSize*2 + 5
	for i := 0; i < total; i++ {
		repo.claimItems = append(repo.claimItems, UsageBillingRetryItem{
			ID:      int64(i + 1),
			Command: &UsageBillingCommand{RequestID: "drain", APIKeyID: int64(i + 1)},
		})
	}
	svc := newUsageBillingRetryServiceForTest(repo)

	require.Equal(t, total, svc.runOnce(context.Background()))
	require.Len(t, repo.completed, total)
	require.Equal(t, 3, repo.claimCalls)
}

func TestUsageBillingRetryService_StopsWhenSameItemsAreReclaimed(t *testing.T) {
	// 应用与数据库时钟不一致时，本轮失败的条目可能被再次领到；按 id 去重后收尾，不能死循环。
	repo := &reclaimingRetryQueueStub{}
	repo.err = context.DeadlineExceeded
	for i := 0; i < usageBillingRetryBatchSize; i++ {
		repo.items = append(repo.items, UsageBillingRetryItem{ID: int64(i + 1), Command: &UsageBillingCommand{RequestID: "loop", APIKeyID: int64(i + 1)}})
	}
	svc := NewUsageBillingRetryService(repo, nil, nil, time.Second, 0)

	require.Equal(t, usageBillingRetryBatchSize, svc.runOnce(context.Background()))
	require.Equal(t, 2, repo.claimCalls)
	require.Len(t, repo.rescheduled, usageBillingRetryBatchSize)
}

// reclaimingRetryQueueStub 每次领取都返回同一批条目，模拟失败条目在同一轮被重新领到。
type reclaimingRetryQueueStub struct {
	usageBillingRetryQueueStub
	items []UsageBillingRetryItem
}

func (s *reclaimingRetryQueueStub) ClaimDueUsageBillingRetries(_ context.Context, _ time.Time, _ int, _ time.Duration) ([]UsageBillingRetryItem, error) {
	s.claimCalls++
	return s.items, nil
}

func TestUsageBillingRetryService_GivesUpAfterDefaultMaxAttempts(t *testing.T) {
	repo := &usageBillingRetryQueueStub{}
	repo.err = context.DeadlineExceeded
	repo.claimItems = []UsageBillingRetryItem{{ID: 16, Command: &UsageBillingCommand{RequestID: "r6", APIKeyID: 1}, Attempts: DefaultUsageBillingRetryMaxAttempts - 1}}
	svc := newUsageBillingRetryServiceForTest(repo)
	require.Equal(t, 10, svc.maxAttempts)

	svc.runOnce(context.Background())
	require.Empty(t, repo.completed)
	require.Len(t, repo.rescheduled, 1)
	require.Equal(t, 10, repo.rescheduled[0].attempts)
	require.True(t, repo.rescheduled[0].failed, "第 10 次仍失败后不再重试")
}

func TestUsageBillingRetryService_HonorsConfiguredMaxAttempts(t *testing.T) {
	repo := &usageBillingRetryQueueStub{}
	repo.err = context.DeadlineExceeded
	repo.claimItems = []UsageBillingRetryItem{
		{ID: 17, Command: &UsageBillingCommand{RequestID: "r7", APIKeyID: 1}, Attempts: 1},
		{ID: 18, Command: &UsageBillingCommand{RequestID: "r8", APIKeyID: 1}, Attempts: 2},
	}
	svc := NewUsageBillingRetryService(repo, nil, nil, time.Second, 3)

	svc.runOnce(context.Background())
	require.Len(t, repo.rescheduled, 2)
	require.False(t, repo.rescheduled[0].failed)
	require.Equal(t, 2, repo.rescheduled[0].attempts)
	require.True(t, repo.rescheduled[1].failed)
	require.Equal(t, 3, repo.rescheduled[1].attempts)
}

func TestUsageBillingRetryService_NonRetryableFailureGivesUpImmediately(t *testing.T) {
	repo := &usageBillingRetryQueueStub{}
	repo.err = ErrUsageBillingRequestConflict
	repo.claimItems = []UsageBillingRetryItem{{ID: 15, Command: &UsageBillingCommand{RequestID: "r5", APIKeyID: 1}}}
	svc := newUsageBillingRetryServiceForTest(repo)

	svc.runOnce(context.Background())
	require.Len(t, repo.rescheduled, 1)
	require.True(t, repo.rescheduled[0].failed)
	require.Equal(t, 1, repo.rescheduled[0].attempts)
}

func TestUsageBillingRetryService_StartIsNoopWithoutQueue(t *testing.T) {
	svc := NewUsageBillingRetryService(&openAIRecordUsageBillingRepoStub{}, nil, nil, time.Millisecond, 0)
	require.Nil(t, svc.queue)
	svc.Start()
	svc.Stop()
}
