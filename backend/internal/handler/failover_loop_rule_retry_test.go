//go:build unit

package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type noopTempUnscheduler struct{}

func (noopTempUnscheduler) TempUnscheduleRetryableError(context.Context, int64, string, *service.UpstreamFailoverError) {
}

// 非 pool-mode 账号的 GetPoolModeRetryCount() 落到 defaultPoolModeRetryCount(3)，
// 而不是 0（零值 Account 的 Type 既非 AccountTypeAPIKey 也非 AccountTypeBedrock，
// IsPoolMode() 恒为 false，兜底走 default）。规则显式配了重试预算时，
// effectiveSameAccountRetryLimit 必须覆盖账号默认值，否则 retry 静默变换号。
func TestEffectiveSameAccountRetryLimitRuleOverridesNonPoolAccount(t *testing.T) {
	account := &service.Account{ID: 11}
	if base := account.GetPoolModeRetryCount(); base != 3 {
		t.Fatalf("precondition: expected non-pool account default budget 3, got %d", base)
	}
	limit := 2
	failoverErr := &service.UpstreamFailoverError{
		RetryableOnSameAccount: true,
		RuleRetryLimit:         &limit,
	}

	if got := effectiveSameAccountRetryLimit(failoverErr, account); got != 2 {
		t.Fatalf("expected rule retry limit 2 to win, got %d", got)
	}
}

// HandleFailoverError 拿到规则预算时必须真的在同账号上重试，而不是换号。
func TestHandleFailoverErrorUsesRuleRetryBudget(t *testing.T) {
	account := &service.Account{ID: 11}
	limit := 2
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:             500,
		RetryableOnSameAccount: true,
		RuleRetryLimit:         &limit,
		NextAccountAction:      service.NextAccountRetry,
	}

	fs := NewFailoverState(3, false)
	action := fs.HandleFailoverError(
		context.Background(), noopTempUnscheduler{},
		account.ID, service.PlatformGemini,
		effectiveSameAccountRetryLimit(failoverErr, account),
		failoverErr,
	)

	if action != FailoverContinue {
		t.Fatalf("expected FailoverContinue, got %v", action)
	}
	if fs.SameAccountRetryCount[account.ID] != 1 {
		t.Fatalf("expected a same-account retry, got count=%d switch=%d",
			fs.SameAccountRetryCount[account.ID], fs.SwitchCount)
	}
	if fs.SwitchCount != 0 {
		t.Fatalf("rule retry must not switch account, got switch_count=%d", fs.SwitchCount)
	}
}
