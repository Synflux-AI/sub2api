package service

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 健康分层：分数只改变账号在候选集内的排序，永不减少候选集。
const (
	// HealthTierHealthy 主池：正常参与调度
	HealthTierHealthy = 0
	// HealthTierDegraded 候选池：仅当主池账号全部不可用/繁忙时使用
	HealthTierDegraded = 1
	// HealthTierProbation 隔离观察：接近熔断，仅当其余账号全部不可用时使用
	HealthTierProbation = 2
)

const (
	healthMaxScore = 100.0

	// 异步写 Redis 的超时，避免错误风暴时 goroutine 堆积
	healthRecordTimeout = 2 * time.Second

	// 本地"带伤账号"标记的保留时间：仅用于过滤成功加分写（健康账号零 Redis 写开销），
	// 过期后最多多写一次 Lua（无害），无需与 Redis TTL 严格一致。
	healthUnhealthyMarkTTL = 5 * time.Minute
)

// HealthScoreEntry Redis 中的原始健康分记录（衰减在读取侧按 ts 现算）
type HealthScoreEntry struct {
	Score     float64
	UpdatedAt time.Time
}

// AccountHealthCache 健康分存储。实现见 repository.NewAccountHealthCache。
type AccountHealthCache interface {
	// ApplyDelta 原子地"衰减到当前时刻 → 加 delta → clamp[0,100]"，返回更新后的分数。
	// 分数回到 100 时删除 key（完全恢复零常驻）。
	ApplyDelta(ctx context.Context, accountID int64, delta float64, halflifeSeconds, ttlSeconds int) (float64, error)
	// GetScoresBatch 批量读取原始分数记录；不存在的账号不出现在结果中（视为满分）。
	GetScoresBatch(ctx context.Context, accountIDs []int64) (map[int64]HealthScoreEntry, error)
}

// AccountHealthService 账号健康度信号层：
// 采集上游错误/成功事件计算 0-100 健康分（指数衰减恢复），
// 调度器把分数映射为候选池分层（tier）做软性降级。
type AccountHealthService struct {
	cache AccountHealthCache
	cfg   *config.Config
	// settingService 提供健康分布尔开关的 DB 动态覆盖（可为 nil，此时回退 config 静态值）
	settingService *SettingService

	// unhealthyMarks 记录本实例观测到的带伤账号（score < 100），
	// 成功加分只对带伤账号写 Redis。value 为过期时间。
	unhealthyMu    sync.RWMutex
	unhealthyMarks map[int64]time.Time
	// unhealthySweepAt 下一次全量清理过期标记的时间（清理限频，避免每次写锁内 O(n) 扫描）
	unhealthySweepAt time.Time
}

// NewAccountHealthService 创建账号健康度服务
func NewAccountHealthService(cache AccountHealthCache, cfg *config.Config, settingService *SettingService) *AccountHealthService {
	return &AccountHealthService{
		cache:          cache,
		cfg:            cfg,
		settingService: settingService,
		unhealthyMarks: make(map[int64]time.Time),
	}
}

func (s *AccountHealthService) schedulingConfig() *config.GatewaySchedulingConfig {
	if s == nil || s.cfg == nil {
		return nil
	}
	return &s.cfg.Gateway.Scheduling
}

// runtimeToggles 返回布尔开关的生效值：settingService 存在时走 DB 动态覆盖
// （stale-while-revalidate，不阻塞热路径），否则回退 config 静态值。
func (s *AccountHealthService) runtimeToggles() SchedulingHealthRuntime {
	if s == nil {
		return SchedulingHealthRuntime{}
	}
	if s.settingService != nil {
		return s.settingService.GetSchedulingHealthRuntime(context.Background())
	}
	cfg := s.schedulingConfig()
	if cfg == nil {
		return SchedulingHealthRuntime{}
	}
	return SchedulingHealthRuntime{
		ScoringEnabled:     cfg.HealthScoringEnabled,
		ShadowMode:         cfg.HealthShadowMode,
		StickyBreakEnabled: cfg.HealthStickyBreakEnabled,
		PriceAwareEnabled:  cfg.PriceAwareEnabled,
	}
}

// Enabled 健康分采集是否开启（含影子模式）
func (s *AccountHealthService) Enabled() bool {
	if s == nil || s.cache == nil || s.schedulingConfig() == nil {
		return false
	}
	return s.runtimeToggles().ScoringEnabled
}

// SortingActive 健康分是否参与调度排序（开启且非影子模式）
func (s *AccountHealthService) SortingActive() bool {
	if s == nil || s.cache == nil || s.schedulingConfig() == nil {
		return false
	}
	rt := s.runtimeToggles()
	return rt.ScoringEnabled && !rt.ShadowMode
}

// RecordPoolMode 池模式账号是否采集健康分
func (s *AccountHealthService) RecordPoolMode() bool {
	cfg := s.schedulingConfig()
	return cfg != nil && cfg.HealthRecordPoolMode
}

// StickyBreakEnabled 粘性会话账号跌入 tier 2 时是否打破粘性
func (s *AccountHealthService) StickyBreakEnabled() bool {
	if s == nil || s.schedulingConfig() == nil {
		return false
	}
	return s.runtimeToggles().StickyBreakEnabled
}

// penaltyForStatus 返回状态码对应的扣分（负数）；0 表示该状态码不计入健康分。
func (s *AccountHealthService) penaltyForStatus(statusCode int) float64 {
	cfg := s.schedulingConfig()
	if cfg == nil {
		return 0
	}
	switch {
	case statusCode == http.StatusTooManyRequests:
		return -cfg.HealthPenalty429
	case statusCode == http.StatusForbidden:
		return -cfg.HealthPenalty403
	case statusCode == http.StatusNotFound:
		return -cfg.HealthPenalty404
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusPaymentRequired:
		return -cfg.HealthPenaltyAuth
	case statusCode == 529:
		return -cfg.HealthPenaltyOverload
	case statusCode >= 500 && statusCode <= 599:
		return -cfg.HealthPenalty5xx
	default:
		return 0
	}
}

// RecordUpstreamError 记录一次上游错误（异步写，不阻塞请求路径）。
func (s *AccountHealthService) RecordUpstreamError(accountID int64, statusCode int) {
	if s == nil || accountID <= 0 || !s.Enabled() {
		return
	}
	penalty := s.penaltyForStatus(statusCode)
	if penalty == 0 {
		return
	}
	s.markUnhealthy(accountID)
	s.applyDeltaAsync(accountID, penalty, "upstream_error", statusCode)
}

// RecordTimeout 记录一次流超时/网络错误（异步写）。
func (s *AccountHealthService) RecordTimeout(accountID int64) {
	if s == nil || accountID <= 0 || !s.Enabled() {
		return
	}
	cfg := s.schedulingConfig()
	if cfg.HealthPenaltyTimeout <= 0 {
		return
	}
	s.markUnhealthy(accountID)
	s.applyDeltaAsync(accountID, -cfg.HealthPenaltyTimeout, "timeout", 0)
}

// RecordSuccess 记录一次成功请求（仅带伤账号才写 Redis，健康账号零开销）。
func (s *AccountHealthService) RecordSuccess(accountID int64) {
	if s == nil || accountID <= 0 || !s.Enabled() {
		return
	}
	cfg := s.schedulingConfig()
	if cfg.HealthSuccessReward <= 0 || !s.isMarkedUnhealthy(accountID) {
		return
	}
	s.applyDeltaAsync(accountID, cfg.HealthSuccessReward, "success", 0)
}

func (s *AccountHealthService) applyDeltaAsync(accountID int64, delta float64, event string, statusCode int) {
	cfg := s.schedulingConfig()
	halflife := cfg.HealthRecoveryHalflifeSeconds
	ttl := cfg.HealthTTLSeconds
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), healthRecordTimeout)
		defer cancel()
		score, err := s.cache.ApplyDelta(ctx, accountID, delta, halflife, ttl)
		if err != nil {
			slog.Debug("account_health_write_failed",
				"account_id", accountID, "event", event, "delta", delta, "error", err)
			return
		}
		if score >= healthMaxScore {
			s.clearUnhealthy(accountID)
		}
		slog.Debug("account_health_updated",
			"account_id", accountID, "event", event, "status_code", statusCode,
			"delta", delta, "score", score, "tier", s.TierForScore(score))
	}()
}

// GetScoresBatch 批量读取衰减后的健康分。Redis 失败或未开启时返回空 map（全员视为满分，fail-open）。
func (s *AccountHealthService) GetScoresBatch(ctx context.Context, accountIDs []int64) map[int64]float64 {
	if s == nil || !s.Enabled() || len(accountIDs) == 0 {
		return nil
	}
	entries, err := s.cache.GetScoresBatch(ctx, accountIDs)
	if err != nil {
		slog.Debug("account_health_read_failed", "accounts", len(accountIDs), "error", err)
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	cfg := s.schedulingConfig()
	now := time.Now()
	scores := make(map[int64]float64, len(entries))
	for accountID, entry := range entries {
		score := decayHealthScore(entry.Score, entry.UpdatedAt, now, cfg.HealthRecoveryHalflifeSeconds)
		scores[accountID] = score
		if score < healthMaxScore {
			s.markUnhealthy(accountID)
		}
	}
	return scores
}

// TierForScore 把健康分映射为候选池分层
func (s *AccountHealthService) TierForScore(score float64) int {
	cfg := s.schedulingConfig()
	if cfg == nil {
		return HealthTierHealthy
	}
	switch {
	case score >= float64(cfg.HealthDegradedThreshold):
		return HealthTierHealthy
	case score >= float64(cfg.HealthCircuitThreshold):
		return HealthTierDegraded
	default:
		return HealthTierProbation
	}
}

// decayHealthScore 按半衰期向 100 回归：decayed = 100 - (100-score) * 0.5^(elapsed/halflife)
func decayHealthScore(score float64, updatedAt, now time.Time, halflifeSeconds int) float64 {
	if score >= healthMaxScore {
		return healthMaxScore
	}
	if halflifeSeconds <= 0 || !now.After(updatedAt) {
		return clampHealthScore(score)
	}
	elapsed := now.Sub(updatedAt).Seconds()
	decayed := healthMaxScore - (healthMaxScore-score)*math.Pow(0.5, elapsed/float64(halflifeSeconds))
	return clampHealthScore(decayed)
}

func clampHealthScore(score float64) float64 {
	if score < 0 {
		return 0
	}
	if score > healthMaxScore {
		return healthMaxScore
	}
	return score
}

func (s *AccountHealthService) markUnhealthy(accountID int64) {
	now := time.Now()
	// 快路径：标记仍然新鲜（剩余 TTL 过半）时只读不写。
	// GetScoresBatch 在调度热路径上对每个带伤账号都会调用本方法，读锁化解写锁竞争。
	s.unhealthyMu.RLock()
	expire, ok := s.unhealthyMarks[accountID]
	sweepDue := now.After(s.unhealthySweepAt)
	s.unhealthyMu.RUnlock()
	if ok && !sweepDue && expire.Sub(now) > healthUnhealthyMarkTTL/2 {
		return
	}

	s.unhealthyMu.Lock()
	// 限频全量清理过期标记（每 TTL/5 至多一次），避免 map 无界增长
	if now.After(s.unhealthySweepAt) {
		for id, exp := range s.unhealthyMarks {
			if now.After(exp) {
				delete(s.unhealthyMarks, id)
			}
		}
		s.unhealthySweepAt = now.Add(healthUnhealthyMarkTTL / 5)
	}
	s.unhealthyMarks[accountID] = now.Add(healthUnhealthyMarkTTL)
	s.unhealthyMu.Unlock()
}

func (s *AccountHealthService) isMarkedUnhealthy(accountID int64) bool {
	s.unhealthyMu.RLock()
	expire, ok := s.unhealthyMarks[accountID]
	s.unhealthyMu.RUnlock()
	return ok && time.Now().Before(expire)
}

func (s *AccountHealthService) clearUnhealthy(accountID int64) {
	s.unhealthyMu.Lock()
	delete(s.unhealthyMarks, accountID)
	s.unhealthyMu.Unlock()
}
