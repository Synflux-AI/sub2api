package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// #248 回归：请求级上游尝试总预算是唯一横跨「同号原地重试 / 换号 / OAuth 429
// 窗口」三条推进路径的闸门。这些用例锁的是「不管推进来自哪条路径，都消耗同一
// 个额度，耗尽即 FailoverExhausted」。

func TestRequestAttemptBudget_UnlimitedWhenNonPositive(t *testing.T) {
	for _, limit := range []int{0, -1} {
		budget := newRequestAttemptBudget(limit)
		for i := 0; i < 100; i++ {
			require.True(t, budget.Consume(), "limit=%d 必须表示不限", limit)
		}
		require.Equal(t, 100, budget.Used())
		require.Equal(t, 0, budget.Limit(), "非正数一律归一化成 0=不限")
	}
}

func TestRequestAttemptBudget_ConsumeCountsAttemptsNotAdvances(t *testing.T) {
	budget := newRequestAttemptBudget(3)

	// limit=N 的口径是「最多 N 次上游尝试」：前 N-1 次失败后还能再推进一次，
	// 第 N 次失败时已经用掉全部额度，不得再推进。
	require.True(t, budget.Consume())
	require.True(t, budget.Consume())
	require.False(t, budget.Consume())
	require.Equal(t, 3, budget.Used())
	require.Equal(t, 3, budget.Limit())
}

func TestMaxUpstreamAttemptsFromConfig(t *testing.T) {
	require.Equal(t, defaultMaxUpstreamAttempts, maxUpstreamAttemptsFromConfig(0, false),
		"没有配置对象时回退默认值")
	require.Equal(t, defaultMaxUpstreamAttempts, maxUpstreamAttemptsFromConfig(99, false),
		"没有配置对象时忽略传入值")
	require.Equal(t, 5, maxUpstreamAttemptsFromConfig(5, true))
	require.Equal(t, 0, maxUpstreamAttemptsFromConfig(0, true),
		"显式 0 是「不限」这个逃生阀，不能当成未配置")
	require.Equal(t, 0, maxUpstreamAttemptsFromConfig(-1, true))
}

func TestFailoverState_AttemptBudgetBoundsSameAccountRetries(t *testing.T) {
	mock := &mockTempUnscheduler{}
	// 预算 3 < 同号重试预算 10：预算必须先咬住。
	fs := NewFailoverState(10, false).WithUpstreamAttemptBudget(3)
	err := newTestFailoverErr(http.StatusTooManyRequests, true, false)
	err.SameAccountRetryDelay = time.Nanosecond

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), mock, 100, "openai", 10, err))
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), mock, 100, "openai", 10, err))
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), mock, 100, "openai", 10, err))

	require.Equal(t, 3, fs.UpstreamAttempts())
	require.Equal(t, 2, fs.SameAccountRetryCount[100])
	require.Zero(t, fs.SwitchCount)
	// 预算耗尽是请求级判定，与账号健康无关：不得顺手临时封禁账号。
	require.Empty(t, mock.calls)
}

func TestFailoverState_AttemptBudgetSharedAcrossRetriesAndSwitches(t *testing.T) {
	mock := &mockTempUnscheduler{}
	fs := NewFailoverState(10, false).WithUpstreamAttemptBudget(4)
	retryErr := newTestFailoverErr(http.StatusTooManyRequests, true, false)
	retryErr.SameAccountRetryDelay = time.Nanosecond
	switchErr := newTestFailoverErr(http.StatusInternalServerError, false, false)

	// 两次同号原地重试 + 一次换号共用同一个额度，第 4 次尝试失败即耗尽。
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), mock, 100, "openai", 2, retryErr))
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), mock, 100, "openai", 2, retryErr))
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), mock, 100, "openai", 2, switchErr))
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), mock, 200, "openai", 2, switchErr))

	require.Equal(t, 4, fs.UpstreamAttempts())
	require.Equal(t, 2, fs.SameAccountRetryCount[100])
	require.Equal(t, 1, fs.SwitchCount, "换号上限是 10，本次只换了 1 次就被总预算咬住")
}

func TestFailoverState_AttemptBudgetBoundsOAuth429Window(t *testing.T) {
	mock := &mockTempUnscheduler{}
	// 复刻 #248 的病态形态：OAuth 429 带一个远未过期的 2 分钟窗口，
	// 同号重试预算被放得很宽。总预算必须替换掉原先的「无限续期」。
	fs := NewFailoverState(10, false).WithUpstreamAttemptBudget(2)
	err := newTestFailoverErr(http.StatusTooManyRequests, true, false)
	err.SameAccountRetryDeadline = time.Now().Add(2 * time.Minute)
	err.SameAccountRetryDelay = time.Nanosecond

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), mock, 81, "openai", 100, err))
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), mock, 81, "openai", 100, err))
	require.Equal(t, 2, fs.UpstreamAttempts())
}

func TestFailoverState_SharedAttemptBudgetSurvivesStateRecreation(t *testing.T) {
	mock := &mockTempUnscheduler{}
	// Anthropic Messages 的兜底分组重试会在同一次请求里重建 FailoverState。
	// 预算若随之重置，「原分组打满 + 兜底分组再打满」能用掉两倍额度。
	budget := newRequestAttemptBudget(3)
	err := newTestFailoverErr(http.StatusInternalServerError, false, false)

	first := NewFailoverState(10, false).WithSharedUpstreamAttemptBudget(&budget)
	require.Equal(t, FailoverContinue, first.HandleFailoverError(context.Background(), mock, 100, "anthropic", 3, err))
	require.Equal(t, FailoverContinue, first.HandleFailoverError(context.Background(), mock, 200, "anthropic", 3, err))

	// 兜底分组：新 FailoverState，但共用同一个预算，只剩最后 1 次尝试。
	fallback := NewFailoverState(10, false).WithSharedUpstreamAttemptBudget(&budget)
	require.Equal(t, FailoverExhausted, fallback.HandleFailoverError(context.Background(), mock, 300, "anthropic", 3, err))
	require.Equal(t, 3, budget.Used())
	require.Zero(t, fallback.SwitchCount, "预算耗尽必须抢在换号之前")
}

func TestFailoverState_AttemptBudgetUnsetKeepsLegacyBehavior(t *testing.T) {
	mock := &mockTempUnscheduler{}
	// 没装配预算的 FailoverState（attempts 为 nil）必须与接入本预算之前逐行同义。
	fs := NewFailoverState(10, false)
	err := newTestFailoverErr(http.StatusInternalServerError, false, false)

	for i := 0; i < 10; i++ {
		require.Equal(t, FailoverContinue,
			fs.HandleFailoverError(context.Background(), mock, int64(i+1), "openai", 3, err))
	}
	require.Equal(t, FailoverExhausted,
		fs.HandleFailoverError(context.Background(), mock, 11, "openai", 3, err),
		"终止应由 MaxSwitches 触发，而不是预算")
	require.Equal(t, 10, fs.SwitchCount)
	require.Zero(t, fs.UpstreamAttempts(), "未装配预算时计数器保持 nil 安全")
}

func TestFailoverState_AttemptBudgetNotConsumedByNonRetryableError(t *testing.T) {
	mock := &mockTempUnscheduler{}
	fs := NewFailoverState(10, false).WithUpstreamAttemptBudget(5)
	err := &service.UpstreamFailoverError{StatusCode: http.StatusBadRequest, NextAccountAction: service.NextAccountStop}

	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), mock, 100, "openai", 3, err))
	require.Zero(t, fs.UpstreamAttempts(),
		"规则判了「不再换号」时请求本来就要终止，不该额外记一次预算消耗")
}
