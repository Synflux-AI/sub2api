//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpsAccountErrorRateRow_ErrorRate(t *testing.T) {
	t.Parallel()

	require.InDelta(t, 0.0, OpsAccountErrorRateRow{Requests: 0, UpstreamErrors: 0}.ErrorRate(), 0.0001)
	require.InDelta(t, 0.0, OpsAccountErrorRateRow{Requests: 0, UpstreamErrors: 5}.ErrorRate(), 0.0001)
	require.InDelta(t, 50.0, OpsAccountErrorRateRow{Requests: 10, UpstreamErrors: 5}.ErrorRate(), 0.0001)
	require.InDelta(t, 13.8, OpsAccountErrorRateRow{Requests: 1000, UpstreamErrors: 138}.ErrorRate(), 0.0001)
}

func newTestMonitor() *AccountErrorRateMonitorService {
	return &AccountErrorRateMonitorService{states: map[int64]*accountBreachState{}}
}

func TestMonitor_UpdateBreaches_ConsecutiveAndReset(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	interval := time.Minute

	require.Equal(t, 1, s.updateBreaches(1, now, interval, true))
	require.Equal(t, 2, s.updateBreaches(1, now.Add(interval), interval, true))
	require.Equal(t, 3, s.updateBreaches(1, now.Add(2*interval), interval, true))

	// 一次未破阈值即清零。
	require.Equal(t, 0, s.updateBreaches(1, now.Add(3*interval), interval, false))

	// 重新累计到 2。
	require.Equal(t, 1, s.updateBreaches(1, now.Add(4*interval), interval, true))
	require.Equal(t, 2, s.updateBreaches(1, now.Add(5*interval), interval, true))

	// 评估间隔异常拉长(>3x)时重置连续计数:本次破阈值只算作 1,而不是延续为 3。
	require.Equal(t, 1, s.updateBreaches(1, now.Add(5*interval+4*interval), interval, true))
}

func TestMonitor_SustainedGate(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	interval := time.Minute
	// SustainedMinutes=3, interval=1m -> 需要连续 3 次破阈值。
	required := requiredSustainedBreaches(3, interval)
	require.Equal(t, 3, required)

	require.Less(t, s.updateBreaches(7, now, interval, true), required)
	require.Less(t, s.updateBreaches(7, now.Add(interval), interval, true), required)
	require.GreaterOrEqual(t, s.updateBreaches(7, now.Add(2*interval), interval, true), required)
}

func TestMonitor_AlertCooldown(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	// 从未告警过 -> 允许。
	require.True(t, s.alertCooldownPassed(1, now, 30))
	s.markAlerted(1, now)

	// 冷却期内 -> 不允许。
	require.False(t, s.alertCooldownPassed(1, now.Add(10*time.Minute), 30))
	// 冷却期满 -> 允许。
	require.True(t, s.alertCooldownPassed(1, now.Add(30*time.Minute), 30))
}

func TestMonitor_DetachReentryAfterCooldown(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	// 首次允许剥离。
	require.True(t, s.shouldDetach(1, now))
	until := s.detachUntil(now, 30) // 30 分钟自愈
	require.Equal(t, now.Add(30*time.Minute), until)
	s.markDetached(1, until)

	// 剥离窗口内不重复剥离。
	require.False(t, s.shouldDetach(1, now.Add(10*time.Minute)))
	// 到期后允许再次剥离。
	require.True(t, s.shouldDetach(1, now.Add(30*time.Minute)))
}

func TestMonitor_DetachUntil_ManualHold(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	// cooldown=0 -> 保持下线(很远的将来),人工模式下不会自动再剥离。
	until := s.detachUntil(now, 0)
	require.True(t, until.After(now.Add(10*365*24*time.Hour)))

	s.markDetached(1, until)
	require.False(t, s.shouldDetach(1, now.Add(365*24*time.Hour)))
}

func TestMonitor_PruneStates(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Now().UTC()
	s.updateBreaches(1, now, time.Minute, true)
	s.updateBreaches(2, now, time.Minute, true)
	s.updateBreaches(3, now, time.Minute, true)

	// 仅账号 1、3 仍出现在最新结果集 -> 账号 2 的状态应被清理。
	s.pruneStates([]OpsAccountErrorRateRow{{AccountID: 1}, {AccountID: 3}}, now)

	s.mu.Lock()
	_, has1 := s.states[1]
	_, has2 := s.states[2]
	_, has3 := s.states[3]
	s.mu.Unlock()
	require.True(t, has1)
	require.False(t, has2)
	require.True(t, has3)
}

func TestMonitor_ResolveClearsCooldownForRelapse(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	// 模拟一次 firing:已告警 + 已剥离到 T+30。
	s.markFiring(1)
	s.markAlerted(1, now)
	s.markDetached(1, s.detachUntil(now, 30))

	// 错误率回落 -> resolve,应清空告警/剥离冷却记忆。
	s.resolveIfFiring(1)

	// 复发(仅 2 分钟后):新一轮事件应能立即重新告警、重新剥离,不被上一轮冷却压制。
	require.True(t, s.alertCooldownPassed(1, now.Add(2*time.Minute), 30))
	require.True(t, s.shouldDetach(1, now.Add(2*time.Minute)))
}

func TestMonitor_ResolveNoopWhenNotFiring(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	// 未 firing 时告警过(理论上不会发生),resolve 不应清掉冷却(只在 firing→resolved 时清)。
	s.markAlerted(1, now)
	s.resolveIfFiring(1)
	require.False(t, s.alertCooldownPassed(1, now.Add(5*time.Minute), 30))
}

func TestMonitor_PruneKeepsStillDetached(t *testing.T) {
	t.Parallel()

	s := newTestMonitor()
	now := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)

	// 账号 9 已被剥离至 T+30,且因无流量从结果集消失;prune 不应清掉它,以免丢失剥离窗口。
	s.markFiring(9)
	s.markDetached(9, now.Add(30*time.Minute))
	s.pruneStates(nil, now)

	s.mu.Lock()
	_, has9 := s.states[9]
	s.mu.Unlock()
	require.True(t, has9)

	// 剥离窗口到期后再 prune 才清理。
	s.pruneStates(nil, now.Add(31*time.Minute))
	s.mu.Lock()
	_, has9after := s.states[9]
	s.mu.Unlock()
	require.False(t, has9after)
}

func TestMonitor_DetachActionText(t *testing.T) {
	t.Parallel()

	require.Equal(t, "仅告警(未自动剥离)", detachActionText(OpsAccountErrorRateMonitorSettings{AutoDetach: false}))
	require.Contains(t, detachActionText(OpsAccountErrorRateMonitorSettings{AutoDetach: true, DetachCooldownMinutes: 30}), "30 分钟后自动回归")
	require.Contains(t, detachActionText(OpsAccountErrorRateMonitorSettings{AutoDetach: true, DetachCooldownMinutes: 0}), "需人工恢复")
}

func TestMonitor_AccountDisplayName(t *testing.T) {
	t.Parallel()

	require.Equal(t, "渠道A", accountDisplayName(OpsAccountErrorRateRow{AccountID: 5, AccountName: "渠道A"}))
	require.Equal(t, "account#5", accountDisplayName(OpsAccountErrorRateRow{AccountID: 5, AccountName: "  "}))
}

func TestMonitor_DetachReason(t *testing.T) {
	t.Parallel()

	row := OpsAccountErrorRateRow{AccountID: 1, AccountName: "x", Requests: 200, UpstreamErrors: 60}
	auto := buildAccountDetachReason(30, 10, 5, row, 30)
	require.Contains(t, auto, "auto-recover in 30m")
	manual := buildAccountDetachReason(30, 10, 5, row, 0)
	require.Contains(t, manual, "manual recovery required")
}
