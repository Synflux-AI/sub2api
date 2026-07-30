package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func healthTestAccountWithLoad(id int64, priority, loadRate int, rateMultiplier float64) accountWithLoad {
	m := rateMultiplier
	return accountWithLoad{
		account:  &Account{ID: id, Priority: priority, RateMultiplier: &m},
		loadInfo: &AccountLoadInfo{AccountID: id, LoadRate: loadRate},
	}
}

func tierByID(tiers map[int64]int) func(*Account) int {
	return func(a *Account) int { return tiers[a.ID] }
}

func idsOf(items []accountWithLoad) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.account.ID)
	}
	return ids
}

// --- filterByBestHealthTier ---

func TestFilterByBestHealthTierPrefersHealthy(t *testing.T) {
	accounts := []accountWithLoad{
		healthTestAccountWithLoad(1, 10, 0, 1),
		healthTestAccountWithLoad(2, 50, 0, 1),
		healthTestAccountWithLoad(3, 50, 0, 1),
	}
	// 低优先级数值的账号 1 带伤（tier 1），健康层必须压过静态 priority
	got := filterByBestHealthTier(accounts, tierByID(map[int64]int{1: HealthTierDegraded}))
	require.ElementsMatch(t, []int64{2, 3}, idsOf(got))
}

func TestFilterByBestHealthTierAllDegradedFallsBack(t *testing.T) {
	accounts := []accountWithLoad{
		healthTestAccountWithLoad(1, 10, 0, 1),
		healthTestAccountWithLoad(2, 50, 0, 1),
	}
	// 全员降级：返回原集合（退化为现状行为，防雪崩）
	got := filterByBestHealthTier(accounts, tierByID(map[int64]int{1: HealthTierProbation, 2: HealthTierProbation}))
	require.ElementsMatch(t, []int64{1, 2}, idsOf(got))
}

func TestFilterByBestHealthTierProbationLastResort(t *testing.T) {
	accounts := []accountWithLoad{
		healthTestAccountWithLoad(1, 10, 0, 1),
		healthTestAccountWithLoad(2, 50, 0, 1),
	}
	got := filterByBestHealthTier(accounts, tierByID(map[int64]int{1: HealthTierProbation, 2: HealthTierDegraded}))
	require.ElementsMatch(t, []int64{2}, idsOf(got))
}

func TestFilterByBestHealthTierNilTierOf(t *testing.T) {
	accounts := []accountWithLoad{
		healthTestAccountWithLoad(1, 10, 0, 1),
		healthTestAccountWithLoad(2, 50, 0, 1),
	}
	require.Len(t, filterByBestHealthTier(accounts, nil), 2)
	require.Empty(t, filterByBestHealthTier(nil, tierByID(nil)))
}

// --- computeCostLoadBuckets / filterByMinCostLoad ---

func newPriceAwareGatewayService(priceWeight float64, guard int) *GatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.PriceAwareEnabled = true
	cfg.Gateway.Scheduling.PriceWeight = priceWeight
	cfg.Gateway.Scheduling.PriceLoadGuardPercent = guard
	return &GatewayService{cfg: cfg}
}

func TestComputeCostLoadBucketsPrefersCheaper(t *testing.T) {
	s := newPriceAwareGatewayService(0.3, 80)
	items := []accountWithLoad{
		healthTestAccountWithLoad(1, 50, 20, 0.8),
		healthTestAccountWithLoad(2, 50, 20, 1.0),
	}
	buckets := s.computeCostLoadBuckets(items)
	require.NotNil(t, buckets)
	// 同负载下便宜账号综合分更低（更优）
	require.Less(t, buckets[1], buckets[2])

	got := filterByMinCostLoad(items, buckets)
	require.Equal(t, []int64{1}, idsOf(got))
}

func TestComputeCostLoadBucketsLoadGuard(t *testing.T) {
	s := newPriceAwareGatewayService(0.3, 80)
	items := []accountWithLoad{
		healthTestAccountWithLoad(1, 50, 85, 0.5), // 便宜但高载：价格优势作废
		healthTestAccountWithLoad(2, 50, 30, 1.0),
	}
	buckets := s.computeCostLoadBuckets(items)
	// 高载账号 bucket = 0.3*100 + 0.7*85 ≈ 90；低载贵账号 = 0.3*100 + 0.7*30 = 51
	require.Greater(t, buckets[1], buckets[2])

	got := filterByMinCostLoad(items, buckets)
	require.Equal(t, []int64{2}, idsOf(got))
}

func TestComputeCostLoadBucketsDisabled(t *testing.T) {
	cfg := &config.Config{}
	s := &GatewayService{cfg: cfg}
	items := []accountWithLoad{healthTestAccountWithLoad(1, 50, 20, 0.8)}
	require.Nil(t, s.computeCostLoadBuckets(items))
	// buckets 为 nil 时 filterByMinCostLoad 原样返回
	require.Len(t, filterByMinCostLoad(items, nil), 1)
}

func TestCostBucketFuncFallsBackToLoadRate(t *testing.T) {
	item := healthTestAccountWithLoad(1, 50, 42, 1)
	require.Equal(t, 42, costBucketFunc(nil)(item))
	require.Equal(t, 7, costBucketFunc(map[int64]int{1: 7})(item))
	require.Equal(t, 42, costBucketFunc(map[int64]int{2: 7})(item))
}

// --- sortRoutingCandidates ---

func TestSortRoutingCandidatesTierBeforePriority(t *testing.T) {
	now := time.Now()
	mk := func(id int64, priority, loadRate int, lastUsed time.Time) accountWithLoad {
		item := healthTestAccountWithLoad(id, priority, loadRate, 1)
		item.account.LastUsedAt = &lastUsed
		return item
	}
	items := []accountWithLoad{
		mk(1, 10, 0, now.Add(-time.Hour)), // 高优先级但带伤
		mk(2, 50, 50, now),                // 健康低优先级
		mk(3, 50, 10, now),                // 健康低优先级低负载
	}
	sortRoutingCandidates(items,
		func(a *Account) int { return a.Priority },
		tierByID(map[int64]int{1: HealthTierDegraded}),
		nil,
	)
	// 健康账号在前（tier 首键），健康组内低负载优先，带伤账号垫底
	require.Equal(t, []int64{3, 2, 1}, idsOf(items))
}

func TestSortRoutingCandidatesBackwardCompatible(t *testing.T) {
	now := time.Now()
	mk := func(id int64, priority, loadRate int, lastUsed time.Time) accountWithLoad {
		item := healthTestAccountWithLoad(id, priority, loadRate, 1)
		item.account.LastUsedAt = &lastUsed
		return item
	}
	items := []accountWithLoad{
		mk(2, 50, 10, now),
		mk(1, 10, 90, now),
	}
	// 旧入口：无 tier/bucket，仍按 优先级 -> 负载 -> LRU
	sortRoutingCandidatesByPriority(items, func(a *Account) int { return a.Priority })
	require.Equal(t, []int64{1, 2}, idsOf(items))
}

// --- healthTierOf / stickyHealthOK ---

func newHealthGatewayService(cfg *config.Config, cache AccountHealthCache) *GatewayService {
	rls := &RateLimitService{cfg: cfg}
	rls.SetAccountHealthService(NewAccountHealthService(cache, cfg))
	return &GatewayService{cfg: cfg, rateLimitService: rls}
}

func TestHealthTierOfUsesPrefetchContext(t *testing.T) {
	cfg := newHealthTestConfig()
	s := newHealthGatewayService(cfg, &stubHealthCache{})

	ctx := context.WithValue(context.Background(), healthScorePrefetchContextKey, map[int64]float64{
		1: 50, // tier 1
		2: 10, // tier 2
	})
	require.Equal(t, HealthTierDegraded, s.healthTierOf(ctx, &Account{ID: 1}))
	require.Equal(t, HealthTierProbation, s.healthTierOf(ctx, &Account{ID: 2}))
	// 无记录 = 主池
	require.Equal(t, HealthTierHealthy, s.healthTierOf(ctx, &Account{ID: 3}))

	// 粘性打破：仅 tier 2
	require.True(t, s.stickyHealthOK(ctx, &Account{ID: 1}))
	require.False(t, s.stickyHealthOK(ctx, &Account{ID: 2}))
	require.True(t, s.stickyHealthOK(ctx, &Account{ID: 3}))

	// 关闭粘性打破开关后 tier 2 也保持粘性
	cfg.Gateway.Scheduling.HealthStickyBreakEnabled = false
	require.True(t, s.stickyHealthOK(ctx, &Account{ID: 2}))
}

func TestHealthTierOfShadowModeInactive(t *testing.T) {
	cfg := newHealthTestConfig()
	cfg.Gateway.Scheduling.HealthShadowMode = true
	s := newHealthGatewayService(cfg, &stubHealthCache{})

	ctx := context.WithValue(context.Background(), healthScorePrefetchContextKey, map[int64]float64{1: 10})
	// 影子模式不参与排序
	require.Equal(t, HealthTierHealthy, s.healthTierOf(ctx, &Account{ID: 1}))
	require.True(t, s.stickyHealthOK(ctx, &Account{ID: 1}))
}

func TestWithHealthPrefetchInjectsScores(t *testing.T) {
	cfg := newHealthTestConfig()
	cache := &stubHealthCache{entries: map[int64]HealthScoreEntry{
		1: {Score: 40, UpdatedAt: time.Now()},
	}}
	s := newHealthGatewayService(cfg, cache)

	ctx := s.withHealthPrefetch(context.Background(), []Account{{ID: 1}, {ID: 2}})
	score, ok := healthScoreFromPrefetchContext(ctx, 1)
	require.True(t, ok)
	require.InDelta(t, 40, score, 0.1)
	_, ok = healthScoreFromPrefetchContext(ctx, 2)
	require.False(t, ok)
}

func TestWithHealthPrefetchDisabledNoop(t *testing.T) {
	cfg := &config.Config{}
	s := newHealthGatewayService(cfg, &stubHealthCache{})
	ctx := s.withHealthPrefetch(context.Background(), []Account{{ID: 1}})
	_, ok := healthScoreFromPrefetchContext(ctx, 1)
	require.False(t, ok)
}
