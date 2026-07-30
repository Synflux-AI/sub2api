package service

import (
	"context"
	"log/slog"
	"math"
)

// 账号健康分调度注入：健康分/价格只改变候选账号的排序，永不减少候选集。
// 全员降级时 filterByBestHealthTier 返回原集合，行为退化为现状（防雪崩）。

type healthScorePrefetchContextKeyType struct{}

var healthScorePrefetchContextKey = healthScorePrefetchContextKeyType{}

func (s *GatewayService) accountHealthService() *AccountHealthService {
	if s == nil {
		return nil
	}
	return s.rateLimitService.HealthService()
}

// withHealthPrefetch 批量预取候选账号健康分写入 context（一次 pipeline 读）。
// 影子模式只记录日志不注入排序；Redis 失败时不注入（全员视为满分，fail-open）。
func (s *GatewayService) withHealthPrefetch(ctx context.Context, accounts []Account) context.Context {
	hs := s.accountHealthService()
	if ctx == nil || hs == nil || !hs.Enabled() || len(accounts) == 0 {
		return ctx
	}
	accountIDs := make([]int64, 0, len(accounts))
	for i := range accounts {
		accountIDs = append(accountIDs, accounts[i].ID)
	}
	scores := hs.GetScoresBatch(ctx, accountIDs)
	if len(scores) == 0 {
		return ctx
	}
	if !hs.SortingActive() {
		// 影子模式：只观察带伤账号的分数分布，不影响排序
		for accountID, score := range scores {
			slog.Info("account_health_shadow",
				"account_id", accountID, "score", score, "tier", hs.TierForScore(score))
		}
		return ctx
	}
	return context.WithValue(ctx, healthScorePrefetchContextKey, scores)
}

func healthScoreFromPrefetchContext(ctx context.Context, accountID int64) (float64, bool) {
	if ctx == nil || accountID <= 0 {
		return 0, false
	}
	m, ok := ctx.Value(healthScorePrefetchContextKey).(map[int64]float64)
	if !ok || len(m) == 0 {
		return 0, false
	}
	v, exists := m[accountID]
	return v, exists
}

// healthTierOf 返回账号的健康分层；未开启排序或无记录时为主池（tier 0）。
func (s *GatewayService) healthTierOf(ctx context.Context, account *Account) int {
	hs := s.accountHealthService()
	if hs == nil || !hs.SortingActive() || account == nil {
		return HealthTierHealthy
	}
	score, ok := healthScoreFromPrefetchContext(ctx, account.ID)
	if !ok {
		return HealthTierHealthy
	}
	return hs.TierForScore(score)
}

// stickyHealthOK 粘性会话健康检查：仅当账号跌入 tier 2（接近熔断）时打破粘性。
// 候选池账号（tier 1）保持粘性——打破粘性有 ForceCacheBilling / 上下文缓存丢失成本。
func (s *GatewayService) stickyHealthOK(ctx context.Context, account *Account) bool {
	hs := s.accountHealthService()
	if hs == nil || !hs.SortingActive() || !hs.StickyBreakEnabled() {
		return true
	}
	return s.healthTierOf(ctx, account) < HealthTierProbation
}

// filterByBestHealthTier 取「存在账号的最优健康层」子集。
// 所有账号同层（含全员降级）时返回原集合，保证候选集永不为空。
func filterByBestHealthTier(accounts []accountWithLoad, tierOf func(*Account) int) []accountWithLoad {
	if len(accounts) <= 1 || tierOf == nil {
		return accounts
	}
	minTier := tierOf(accounts[0].account)
	sameTier := true
	for _, acc := range accounts[1:] {
		t := tierOf(acc.account)
		if t != minTier {
			sameTier = false
		}
		if t < minTier {
			minTier = t
		}
	}
	if sameTier {
		return accounts
	}
	result := make([]accountWithLoad, 0, len(accounts))
	for _, acc := range accounts {
		if tierOf(acc.account) == minTier {
			result = append(result, acc)
		}
	}
	return result
}

// computeCostLoadBuckets 计算每个账号的「价格×负载」综合分桶（整数，越小越优）：
//
//	bucket = round( PriceWeight * normalizedPrice * 100 + (1 - PriceWeight) * loadRate )
//	normalizedPrice = rateMultiplier / max(rateMultiplier in set)
//
// 负载守卫：loadRate >= PriceLoadGuardPercent 的账号价格优势作废（normalizedPrice 视为 1），
// 高载时退回纯负载均衡，防止便宜账号被打爆。
// 价格感知未开启或倍率全为 0 时返回 nil（调用方退回纯负载比较）。
func (s *GatewayService) computeCostLoadBuckets(items []accountWithLoad) map[int64]int {
	cfg := s.schedulingConfig()
	if !cfg.PriceAwareEnabled || len(items) == 0 {
		return nil
	}
	maxMultiplier := 0.0
	for _, item := range items {
		if m := item.account.BillingRateMultiplier(); m > maxMultiplier {
			maxMultiplier = m
		}
	}
	if maxMultiplier <= 0 {
		return nil
	}
	weight := cfg.PriceWeight
	guard := cfg.PriceLoadGuardPercent
	buckets := make(map[int64]int, len(items))
	for _, item := range items {
		normPrice := item.account.BillingRateMultiplier() / maxMultiplier
		if normPrice > 1 {
			normPrice = 1
		}
		if guard > 0 && item.loadInfo.LoadRate >= guard {
			normPrice = 1
		}
		score := weight*normPrice*100 + (1-weight)*float64(item.loadInfo.LoadRate)
		buckets[item.account.ID] = int(math.Round(score))
	}
	return buckets
}

// filterByMinCostLoad 过滤出综合分桶最小的账号集合；buckets 为 nil 时原样返回。
func filterByMinCostLoad(accounts []accountWithLoad, buckets map[int64]int) []accountWithLoad {
	if len(accounts) <= 1 || buckets == nil {
		return accounts
	}
	bucketOf := costBucketFunc(buckets)
	minBucket := bucketOf(accounts[0])
	for _, acc := range accounts[1:] {
		if b := bucketOf(acc); b < minBucket {
			minBucket = b
		}
	}
	result := make([]accountWithLoad, 0, len(accounts))
	for _, acc := range accounts {
		if bucketOf(acc) == minBucket {
			result = append(result, acc)
		}
	}
	return result
}

// costBucketFunc 返回账号的排序分桶取值函数：价格感知未开启（buckets 为 nil）时退回负载率。
func costBucketFunc(buckets map[int64]int) func(accountWithLoad) int {
	if buckets == nil {
		return func(item accountWithLoad) int { return item.loadInfo.LoadRate }
	}
	return func(item accountWithLoad) int {
		if b, ok := buckets[item.account.ID]; ok {
			return b
		}
		return item.loadInfo.LoadRate
	}
}
