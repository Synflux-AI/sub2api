//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// modelRPMCacheStub 记录每个 (ruleID, userID) 桶的递增次数，并可注入错误。
type modelRPMCacheStub struct {
	mu sync.Mutex

	counts map[string]int
	calls  []modelRPMIncrCall

	minute      int64
	minuteErr   error
	minuteCalls int32
	incrErr     error
}

type modelRPMIncrCall struct {
	ruleID int64
	userID int64
	minute int64
}

func newModelRPMCacheStub() *modelRPMCacheStub {
	return &modelRPMCacheStub{counts: map[string]int{}, minute: 29000000}
}

func (s *modelRPMCacheStub) MinuteTimestamp(_ context.Context) (int64, error) {
	atomic.AddInt32(&s.minuteCalls, 1)
	if s.minuteErr != nil {
		return 0, s.minuteErr
	}
	return s.minute, nil
}

func (s *modelRPMCacheStub) IncrementRuleRPM(_ context.Context, ruleID, userID, minute int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, modelRPMIncrCall{ruleID: ruleID, userID: userID, minute: minute})
	if s.incrErr != nil {
		return 0, s.incrErr
	}
	key := fmt.Sprintf("%d:%d", ruleID, userID)
	s.counts[key]++
	return s.counts[key], nil
}

func (s *modelRPMCacheStub) GetRuleRPM(_ context.Context, ruleID, userID, _ int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[fmt.Sprintf("%d:%d", ruleID, userID)], nil
}

func (s *modelRPMCacheStub) incrCalls() []modelRPMIncrCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]modelRPMIncrCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// modelRPMRuleSnapshotStub 直接返回固定规则快照，跳过 repo/缓存层。
type modelRPMRuleSnapshotStub struct {
	rules []ModelRPMRule
}

func (s *modelRPMRuleSnapshotStub) Snapshot(_ context.Context) []ModelRPMRule {
	sorted := make([]ModelRPMRule, len(s.rules))
	copy(sorted, s.rules)
	sortModelRPMRules(sorted)
	return sorted
}

func newBillingServiceForModelRPM(t *testing.T, rules []ModelRPMRule, cache ModelRPMCache) *BillingCacheService {
	t.Helper()
	svc := NewBillingCacheService(nil, nil, nil, nil, &userRPMCacheStub{}, &rpmOverrideRepoStub{}, &config.Config{}, nil)
	svc.SetModelRPMLimiter(&modelRPMRuleSnapshotStub{rules: rules}, cache)
	t.Cleanup(svc.Stop)
	return svc
}

func modelCtx(model string) context.Context {
	return context.WithValue(context.Background(), ctxkey.Model, model)
}

func modelRPMTargetID(v int64) *int64 { return &v }

func TestCheckModelRPM_UserScopeCountsPerUser(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{{
		ID: 1, Name: "opus", ModelPattern: "claude-opus-4", Scope: ModelRPMScopeUser,
		TargetType: ModelRPMTargetAll, RPMLimit: 2, Enabled: true,
	}}
	svc := newBillingServiceForModelRPM(t, rules, cache)
	ctx := modelCtx("claude-opus-4")

	userA := &User{ID: 11}
	userB := &User{ID: 22}

	require.NoError(t, svc.checkModelRPM(ctx, userA, nil))
	require.NoError(t, svc.checkModelRPM(ctx, userA, nil))
	require.ErrorIs(t, svc.checkModelRPM(ctx, userA, nil), ErrModelRPMExceeded)

	// 另一个用户有独立配额，不受 userA 打满影响。
	require.NoError(t, svc.checkModelRPM(ctx, userB, nil))

	calls := cache.incrCalls()
	require.Len(t, calls, 4)
	require.Equal(t, int64(11), calls[0].userID)
	require.Equal(t, int64(22), calls[3].userID)
	require.EqualValues(t, 4, atomic.LoadInt32(&cache.minuteCalls), "每次检查只取一次分钟戳")
}

func TestCheckModelRPM_GlobalScopeSharesOnePool(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{{
		ID: 7, ModelPattern: "gpt-5", Scope: ModelRPMScopeGlobal,
		TargetType: ModelRPMTargetAll, RPMLimit: 2, Enabled: true,
	}}
	svc := newBillingServiceForModelRPM(t, rules, cache)
	ctx := modelCtx("gpt-5")

	require.NoError(t, svc.checkModelRPM(ctx, &User{ID: 1}, nil))
	require.NoError(t, svc.checkModelRPM(ctx, &User{ID: 2}, nil))
	// 全站共享一池：第三个用户也被同一个桶拦下。
	require.ErrorIs(t, svc.checkModelRPM(ctx, &User{ID: 3}, nil), ErrModelRPMExceeded)

	for _, call := range cache.incrCalls() {
		require.EqualValues(t, 0, call.userID, "global scope 应走全局桶（userID=0）")
	}
}

func TestCheckModelRPM_TrailingWildcardAndNormalization(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{{
		ID: 3, ModelPattern: "claude-opus-", Scope: ModelRPMScopeUser,
		TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true,
	}}
	rules[0].ModelPattern = "claude-opus-*"
	svc := newBillingServiceForModelRPM(t, rules, cache)

	// 大小写与首尾空白都应归一化后再匹配。
	require.NoError(t, svc.checkModelRPM(modelCtx("  Claude-Opus-4-20250101  "), &User{ID: 1}, nil))
	require.ErrorIs(t, svc.checkModelRPM(modelCtx("CLAUDE-OPUS-4[1M]"), &User{ID: 1}, nil), ErrModelRPMExceeded)

	// 前缀不匹配的模型不进桶。
	require.NoError(t, svc.checkModelRPM(modelCtx("claude-sonnet-4"), &User{ID: 1}, nil))
	require.Len(t, cache.incrCalls(), 2)
}

func TestCheckModelRPM_MultipleRulesEarlyReturnStopsLaterBuckets(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{
		{ID: 1, ModelPattern: "opus", Scope: ModelRPMScopeGlobal, TargetType: ModelRPMTargetAll, RPMLimit: 100, Enabled: true},
		{ID: 2, ModelPattern: "opus", Scope: ModelRPMScopeUser, TargetType: ModelRPMTargetUser, TargetID: modelRPMTargetID(5), RPMLimit: 1, Enabled: true},
	}
	svc := newBillingServiceForModelRPM(t, rules, cache)
	ctx := modelCtx("opus")
	user := &User{ID: 5}

	require.NoError(t, svc.checkModelRPM(ctx, user, nil))
	require.ErrorIs(t, svc.checkModelRPM(ctx, user, nil), ErrModelRPMExceeded)

	calls := cache.incrCalls()
	// 具体度降序：user 规则(id=2)先判；第二次它就超了，all 规则(id=1)不该再被 INCR。
	require.Equal(t, []modelRPMIncrCall{
		{ruleID: 2, userID: 5, minute: cache.minute},
		{ruleID: 1, userID: 0, minute: cache.minute},
		{ruleID: 2, userID: 5, minute: cache.minute},
	}, calls)
}

func TestCheckModelRPM_TargetFiltering(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{
		{ID: 1, ModelPattern: "m", Scope: ModelRPMScopeUser, TargetType: ModelRPMTargetGroup, TargetID: modelRPMTargetID(9), RPMLimit: 1, Enabled: true},
		{ID: 2, ModelPattern: "m", Scope: ModelRPMScopeUser, TargetType: ModelRPMTargetUser, TargetID: modelRPMTargetID(9), RPMLimit: 1, Enabled: true},
	}
	svc := newBillingServiceForModelRPM(t, rules, cache)
	ctx := modelCtx("m")

	// 用户 1 在分组 8：两条规则都不命中。
	require.NoError(t, svc.checkModelRPM(ctx, &User{ID: 1}, &Group{ID: 8}))
	require.Empty(t, cache.incrCalls())

	// 用户 1 在分组 9：只命中分组规则。
	require.NoError(t, svc.checkModelRPM(ctx, &User{ID: 1}, &Group{ID: 9}))
	require.Len(t, cache.incrCalls(), 1)
	require.EqualValues(t, 1, cache.incrCalls()[0].ruleID)

	// 用户 9 无分组：只命中用户规则。
	require.NoError(t, svc.checkModelRPM(ctx, &User{ID: 9}, nil))
	require.Len(t, cache.incrCalls(), 2)
	require.EqualValues(t, 2, cache.incrCalls()[1].ruleID)
}

func TestCheckModelRPM_MissingModelSkipsAndCounts(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{{
		ID: 1, ModelPattern: "any", Scope: ModelRPMScopeUser,
		TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true,
	}}
	svc := newBillingServiceForModelRPM(t, rules, cache)

	before := ModelRPMSkippedNoModelCount()
	require.NoError(t, svc.checkModelRPM(context.Background(), &User{ID: 1}, nil))
	require.NoError(t, svc.checkModelRPM(modelCtx("   "), &User{ID: 1}, nil))

	require.Empty(t, cache.incrCalls(), "取不到模型名时不应产生任何计数")
	require.EqualValues(t, before+2, ModelRPMSkippedNoModelCount(),
		"取不到模型名要计数，避免新增 handler 静默漏网")
}

func TestCheckModelRPM_SkipFlagBypassesModelRules(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{{
		ID: 1, ModelPattern: "opus", Scope: ModelRPMScopeUser,
		TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true,
	}}
	svc := newBillingServiceForModelRPM(t, rules, cache)

	ctx := WithoutModelRPMLimit(modelCtx("opus"))
	for i := 0; i < 5; i++ {
		require.NoError(t, svc.checkModelRPM(ctx, &User{ID: 1}, nil))
	}
	require.Empty(t, cache.incrCalls(), "兜底重查不应重复消耗模型配额")
	require.EqualValues(t, 0, atomic.LoadInt32(&cache.minuteCalls))
}

func TestCheckModelRPM_RedisErrorsFailOpen(t *testing.T) {
	rules := []ModelRPMRule{{
		ID: 1, ModelPattern: "opus", Scope: ModelRPMScopeUser,
		TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true,
	}}

	timeCache := newModelRPMCacheStub()
	timeCache.minuteErr = errors.New("redis TIME down")
	svc := newBillingServiceForModelRPM(t, rules, timeCache)
	require.NoError(t, svc.checkModelRPM(modelCtx("opus"), &User{ID: 1}, nil))
	require.Empty(t, timeCache.incrCalls())

	incrCache := newModelRPMCacheStub()
	incrCache.incrErr = errors.New("redis INCR down")
	svc2 := newBillingServiceForModelRPM(t, rules, incrCache)
	for i := 0; i < 3; i++ {
		require.NoError(t, svc2.checkModelRPM(modelCtx("opus"), &User{ID: 1}, nil))
	}
}

func TestCheckModelRPM_NoLimiterConfiguredIsNoop(t *testing.T) {
	svc := NewBillingCacheService(nil, nil, nil, nil, &userRPMCacheStub{}, &rpmOverrideRepoStub{}, &config.Config{}, nil)
	t.Cleanup(svc.Stop)
	require.NoError(t, svc.checkModelRPM(modelCtx("opus"), &User{ID: 1}, nil))
}

func TestCheckRPM_ModelRulesEvaluatedBeforeGroupAndUser(t *testing.T) {
	cache := newModelRPMCacheStub()
	rules := []ModelRPMRule{{
		ID: 1, ModelPattern: "opus", Scope: ModelRPMScopeUser,
		TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true,
	}}
	userCache := &userRPMCacheStub{}
	svc := NewBillingCacheService(nil, nil, nil, nil, userCache, &rpmOverrideRepoStub{}, &config.Config{}, nil)
	svc.SetModelRPMLimiter(&modelRPMRuleSnapshotStub{rules: rules}, cache)
	t.Cleanup(svc.Stop)

	ctx := modelCtx("opus")
	user := &User{ID: 1, RPMLimit: 100}
	group := &Group{ID: 2, RPMLimit: 100}

	require.NoError(t, svc.checkRPM(ctx, user, group))
	require.ErrorIs(t, svc.checkRPM(ctx, user, group), ErrModelRPMExceeded)

	// 模型规则排在最前：被它拒掉的请求不应再消耗 group / user 配额。
	require.EqualValues(t, 1, atomic.LoadInt32(&userCache.userGroupCalls))
	require.EqualValues(t, 1, atomic.LoadInt32(&userCache.userCalls))
}
