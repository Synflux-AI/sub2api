package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newHealthTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.HealthScoringEnabled = true
	cfg.Gateway.Scheduling.HealthShadowMode = false
	cfg.Gateway.Scheduling.HealthDegradedThreshold = 70
	cfg.Gateway.Scheduling.HealthCircuitThreshold = 30
	cfg.Gateway.Scheduling.HealthRecoveryHalflifeSeconds = 600
	cfg.Gateway.Scheduling.HealthSuccessReward = 2
	cfg.Gateway.Scheduling.HealthPenalty429 = 12
	cfg.Gateway.Scheduling.HealthPenalty403 = 35
	cfg.Gateway.Scheduling.HealthPenalty404 = 8
	cfg.Gateway.Scheduling.HealthPenaltyAuth = 25
	cfg.Gateway.Scheduling.HealthPenalty5xx = 20
	cfg.Gateway.Scheduling.HealthPenaltyOverload = 15
	cfg.Gateway.Scheduling.HealthPenaltyTimeout = 10
	cfg.Gateway.Scheduling.HealthTTLSeconds = 1800
	cfg.Gateway.Scheduling.HealthStickyBreakEnabled = true
	cfg.Gateway.Scheduling.HealthRecordPoolMode = true
	return cfg
}

type stubHealthCache struct {
	mu      sync.Mutex
	entries map[int64]HealthScoreEntry
	deltas  []float64
	err     error
}

func (s *stubHealthCache) ApplyDelta(_ context.Context, _ int64, delta float64, _, _ int) (float64, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.mu.Lock()
	s.deltas = append(s.deltas, delta)
	s.mu.Unlock()
	return 100 + delta, nil
}

func (s *stubHealthCache) GetScoresBatch(_ context.Context, _ []int64) (map[int64]HealthScoreEntry, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.entries, nil
}

func (s *stubHealthCache) deltaCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deltas)
}

func (s *stubHealthCache) deltaAt(i int) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deltas[i]
}

func TestAccountHealthPenaltyForStatus(t *testing.T) {
	svc := NewAccountHealthService(&stubHealthCache{}, newHealthTestConfig())

	require.Equal(t, -12.0, svc.penaltyForStatus(http.StatusTooManyRequests))
	require.Equal(t, -35.0, svc.penaltyForStatus(http.StatusForbidden))
	require.Equal(t, -8.0, svc.penaltyForStatus(http.StatusNotFound))
	require.Equal(t, -25.0, svc.penaltyForStatus(http.StatusUnauthorized))
	require.Equal(t, -25.0, svc.penaltyForStatus(http.StatusPaymentRequired))
	require.Equal(t, -15.0, svc.penaltyForStatus(529))
	require.Equal(t, -20.0, svc.penaltyForStatus(http.StatusBadGateway))
	require.Equal(t, -20.0, svc.penaltyForStatus(http.StatusServiceUnavailable))
	require.Equal(t, -20.0, svc.penaltyForStatus(http.StatusInternalServerError))
	// 客户端错误（400）不计入健康分
	require.Equal(t, 0.0, svc.penaltyForStatus(http.StatusBadRequest))
}

func TestAccountHealthTierForScore(t *testing.T) {
	svc := NewAccountHealthService(&stubHealthCache{}, newHealthTestConfig())

	require.Equal(t, HealthTierHealthy, svc.TierForScore(100))
	require.Equal(t, HealthTierHealthy, svc.TierForScore(70))
	require.Equal(t, HealthTierDegraded, svc.TierForScore(69.9))
	require.Equal(t, HealthTierDegraded, svc.TierForScore(30))
	require.Equal(t, HealthTierProbation, svc.TierForScore(29.9))
	require.Equal(t, HealthTierProbation, svc.TierForScore(0))
}

func TestDecayHealthScore(t *testing.T) {
	now := time.Now()

	// 一个半衰期：60 → 80
	got := decayHealthScore(60, now.Add(-600*time.Second), now, 600)
	require.InDelta(t, 80, got, 0.01)

	// 两个半衰期：60 → 90
	got = decayHealthScore(60, now.Add(-1200*time.Second), now, 600)
	require.InDelta(t, 90, got, 0.01)

	// 无时间流逝不衰减
	got = decayHealthScore(60, now, now, 600)
	require.InDelta(t, 60, got, 0.001)

	// 满分恒为满分
	require.Equal(t, 100.0, decayHealthScore(100, now.Add(-time.Hour), now, 600))

	// clamp
	require.Equal(t, 0.0, decayHealthScore(-5, now, now, 600))
}

func TestAccountHealthEnabledGating(t *testing.T) {
	cfg := newHealthTestConfig()
	svc := NewAccountHealthService(&stubHealthCache{}, cfg)
	require.True(t, svc.Enabled())
	require.True(t, svc.SortingActive())

	cfg.Gateway.Scheduling.HealthShadowMode = true
	require.True(t, svc.Enabled())
	require.False(t, svc.SortingActive())

	cfg.Gateway.Scheduling.HealthScoringEnabled = false
	require.False(t, svc.Enabled())
	require.False(t, svc.SortingActive())

	// nil 服务安全
	var nilSvc *AccountHealthService
	require.False(t, nilSvc.Enabled())
	require.False(t, nilSvc.SortingActive())
	nilSvc.RecordUpstreamError(1, 500)
	nilSvc.RecordSuccess(1)
	nilSvc.RecordTimeout(1)
	require.Nil(t, nilSvc.GetScoresBatch(context.Background(), []int64{1}))
}

func TestAccountHealthGetScoresBatchFailOpen(t *testing.T) {
	svc := NewAccountHealthService(&stubHealthCache{err: context.DeadlineExceeded}, newHealthTestConfig())
	// Redis 失败 → 返回空（全员视为满分）
	require.Nil(t, svc.GetScoresBatch(context.Background(), []int64{1, 2}))
}

func TestAccountHealthGetScoresBatchDecaysAndMarks(t *testing.T) {
	now := time.Now()
	cache := &stubHealthCache{entries: map[int64]HealthScoreEntry{
		1: {Score: 60, UpdatedAt: now.Add(-600 * time.Second)},
		2: {Score: 20, UpdatedAt: now},
	}}
	svc := NewAccountHealthService(cache, newHealthTestConfig())

	scores := svc.GetScoresBatch(context.Background(), []int64{1, 2, 3})
	require.Len(t, scores, 2)
	require.InDelta(t, 80, scores[1], 0.1)
	require.InDelta(t, 20, scores[2], 0.1)

	// 带伤账号被本地标记：成功加分会写 Redis
	require.True(t, svc.isMarkedUnhealthy(1))
	require.True(t, svc.isMarkedUnhealthy(2))
	require.False(t, svc.isMarkedUnhealthy(3))
}

func TestAccountHealthRecordSuccessOnlyForUnhealthy(t *testing.T) {
	cache := &stubHealthCache{}
	svc := NewAccountHealthService(cache, newHealthTestConfig())

	// 未标记带伤：成功不写
	svc.RecordSuccess(1)
	require.Equal(t, 0, cache.deltaCount())

	svc.markUnhealthy(1)
	svc.RecordSuccess(1)
	require.Eventually(t, func() bool {
		return cache.deltaCount() == 1
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, 2.0, cache.deltaAt(0))
}

func TestAccountHealthRecordUpstreamErrorAsync(t *testing.T) {
	cache := &stubHealthCache{}
	svc := NewAccountHealthService(cache, newHealthTestConfig())

	svc.RecordUpstreamError(1, http.StatusBadGateway)
	require.Eventually(t, func() bool {
		return cache.deltaCount() == 1
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, -20.0, cache.deltaAt(0))
	require.True(t, svc.isMarkedUnhealthy(1))

	// 不计分的状态码不写
	svc.RecordUpstreamError(2, http.StatusBadRequest)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, 1, cache.deltaCount())
}
