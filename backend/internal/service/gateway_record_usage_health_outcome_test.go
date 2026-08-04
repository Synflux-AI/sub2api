//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newHealthOutcomeRecordUsageService 组一个带真实 AccountHealthService 的网关服务。
// 账号先标记成带伤：RecordSuccess 只对带伤账号写 Redis，这正是问题现场——
// 被扣到带伤的账号靠截断流一次次回血。
func newHealthOutcomeRecordUsageService(t *testing.T, accountID int64) (*GatewayService, *stubHealthCache) {
	t.Helper()
	cache := &stubHealthCache{}
	health := NewAccountHealthService(cache, newHealthTestConfig(), nil)
	health.markUnhealthy(accountID)

	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(
		&openAIRecordUsageLogRepoStub{},
		&openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}},
		&openAIRecordUsageUserRepoStub{},
		&openAIRecordUsageSubRepoStub{},
	)
	svc.rateLimitService = &RateLimitService{healthService: health}
	return svc, cache
}

func recordHealthOutcomeUsage(t *testing.T, svc *GatewayService, accountID int64, result *ForwardResult) {
	t.Helper()
	require.NoError(t, svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result:  result,
		APIKey:  &APIKey{ID: 501, Quota: 100},
		User:    &User{ID: 601},
		Account: &Account{ID: accountID},
	}))
}

func healthOutcomeStreamResult(streamIncomplete, clientDisconnect bool) *ForwardResult {
	return &ForwardResult{
		RequestID:        "health_outcome",
		Usage:            ClaudeUsage{InputTokens: 10, OutputTokens: 6},
		Model:            "claude-sonnet-4-6",
		Stream:           true,
		Duration:         time.Second,
		StreamIncomplete: streamIncomplete,
		ClientDisconnect: clientDisconnect,
	}
}

// 上游流被截断（200 但缺 terminal 事件）时，Forward 会把已观测到的 usage 和错误
// 一起返回，handler 照常提交用量——于是这次「失败」被计费管线记成了一次成功，
// 给带伤账号回血。必须改成按流中断扣分。
func TestGatewayServiceRecordUsage_StreamIncompletePenalizesHealth(t *testing.T) {
	const accountID = 701
	svc, cache := newHealthOutcomeRecordUsageService(t, accountID)

	recordHealthOutcomeUsage(t, svc, accountID, healthOutcomeStreamResult(true, false))

	require.Eventually(t, func() bool { return cache.deltaCount() == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, -10.0, cache.deltaAt(0), "应按 HealthPenaltyTimeout 扣分，而不是 +HealthSuccessReward")
}

// 客户端主动断开走的是同一个出口，但账号没做错任何事：既不回血也不扣分。
func TestGatewayServiceRecordUsage_ClientDisconnectIsHealthNeutral(t *testing.T) {
	const accountID = 702
	svc, cache := newHealthOutcomeRecordUsageService(t, accountID)

	recordHealthOutcomeUsage(t, svc, accountID, healthOutcomeStreamResult(true, true))

	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 0, cache.deltaCount(), "客户端断开不应影响账号健康分")
}

// 完整成功的请求仍要回血，别把回血机制一起改没了。
func TestGatewayServiceRecordUsage_CompleteStreamStillRewardsHealth(t *testing.T) {
	const accountID = 703
	svc, cache := newHealthOutcomeRecordUsageService(t, accountID)

	recordHealthOutcomeUsage(t, svc, accountID, healthOutcomeStreamResult(false, false))

	require.Eventually(t, func() bool { return cache.deltaCount() == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, 2.0, cache.deltaAt(0))
}
