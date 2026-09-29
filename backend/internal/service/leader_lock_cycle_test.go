//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// ---------- tryAcquireCycleLeaderLock ----------

func (f *fakeLeaderLockCache) ttlOf(key string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTTL[key]
}

func TestCycleLeaderLockHold(t *testing.T) {
	require.Equal(t, 54*time.Second, cycleLeaderLockHold(time.Minute))
	require.Zero(t, cycleLeaderLockHold(0))
	require.Less(t, cycleLeaderLockHold(5*time.Minute), 5*time.Minute, "hold 必须短于周期，单副本下一次 tick 才能重新抢到锁")
}

// 本轮很快跑完时，锁仍持有到 hold 结束：同一周期内其他副本的 tick 都要跳过；
// hold 过后任意副本（含自己）都能在下个周期拿到锁。
func TestCycleLeaderLock_HeldUntilCycleEndThenReleased(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	ctx := context.Background()
	const key = "cycle:test:hold"
	const hold = 150 * time.Millisecond

	releaseA, ok := tryAcquireCycleLeaderLock(ctx, cache, nil, key, "A", hold, time.Minute)
	require.True(t, ok)
	releaseA()

	_, okB := tryAcquireCycleLeaderLock(ctx, cache, nil, key, "B", hold, time.Minute)
	require.False(t, okB, "本周期内 A 已执行过，B 必须跳过")
	require.Equal(t, "A", cache.heldBy(key))

	require.Eventually(t, func() bool { return cache.heldBy(key) == "" }, 2*time.Second, 5*time.Millisecond,
		"hold 结束后锁应被释放")

	releaseA2, okA2 := tryAcquireCycleLeaderLock(ctx, cache, nil, key, "A", hold, time.Minute)
	require.True(t, okA2, "下个周期同一实例必须能重新拿到锁（单副本不漏跑）")
	releaseA2()
}

// 本轮跑得比 hold 还久时，结束即释放，不额外占用。
func TestCycleLeaderLock_LongRunReleasesImmediately(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	const key = "cycle:test:long"

	release, ok := tryAcquireCycleLeaderLock(context.Background(), cache, nil, key, "A", 10*time.Millisecond, time.Minute)
	require.True(t, ok)
	time.Sleep(30 * time.Millisecond)
	release()
	require.Empty(t, cache.heldBy(key))
}

// 持有者崩溃（从不 release）后，锁在 TTL 到期时可被其他副本接管。
func TestCycleLeaderLock_CrashedHolderTakenOverAfterTTL(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	cache := &fakeLeaderLockCache{now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	const key = "cycle:test:crash"
	const ttl = 5 * time.Minute

	_, ok := tryAcquireCycleLeaderLock(context.Background(), cache, nil, key, "A", time.Minute, ttl)
	require.True(t, ok)
	// A 崩溃：不调用 release。

	advance(ttl - time.Second)
	_, okB := tryAcquireCycleLeaderLock(context.Background(), cache, nil, key, "B", time.Minute, ttl)
	require.False(t, okB, "TTL 未到期前不可接管")

	advance(2 * time.Second)
	releaseB, okB := tryAcquireCycleLeaderLock(context.Background(), cache, nil, key, "B", time.Minute, ttl)
	require.True(t, okB, "TTL 到期后必须可被接管")
	require.Equal(t, "B", cache.heldBy(key))
	releaseB()
}

func TestCycleLeaderLock_TTLRaisedToHold(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	const key = "cycle:test:ttl"
	release, ok := tryAcquireCycleLeaderLock(context.Background(), cache, nil, key, "A", 10*time.Minute, time.Minute)
	require.True(t, ok)
	require.Equal(t, 10*time.Minute, cache.ttlOf(key), "TTL 小于 hold 时锁会提前过期，必须抬到 hold")
	_ = release
}

func TestCycleLeaderLock_NoBackendAndCacheErrorRunUngated(t *testing.T) {
	release, ok := tryAcquireCycleLeaderLock(context.Background(), nil, nil, "k", "A", time.Minute, time.Minute)
	require.True(t, ok)
	require.NotPanics(t, release)

	cache := &fakeLeaderLockCache{acquireErr: errors.New("redis down")}
	release, ok = tryAcquireCycleLeaderLock(context.Background(), cache, nil, "k", "A", time.Minute, time.Minute)
	require.True(t, ok, "Redis 报错且无 DB 时不设闸门，任务不能被饿死")
	require.NotPanics(t, release)
}

// ---------- 补锁的任务：未拿到锁则跳过本轮 ----------

func TestBatchImageCleanup_ScheduledRunSkipsWhenPeerHoldsLock(t *testing.T) {
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	expired := now.Add(-time.Minute)
	setup := func() (*BatchImageCleanupService, *publicBatchImageProvider) {
		svc, repo, provider := newTestBatchImageCleanupService()
		provider.cleanupErr = nil
		repo.jobs["imgbatch_cleanup"].FinishedAt = &old
		repo.jobs["imgbatch_cleanup"].OutputExpiresAt = &expired
		return svc, provider
	}

	t.Run("peer holds lock", func(t *testing.T) {
		svc, provider := setup()
		cache := &fakeLeaderLockCache{}
		_, _ = cache.TryAcquireLeaderLock(context.Background(), batchImageCleanupLeaderLockKey, "peer", time.Hour)
		svc.SetLeaderLock(cache, nil)

		svc.runScheduledOnce(context.Background())
		require.Empty(t, provider.cleanupTargets, "非 leader 不得执行清理")
	})

	t.Run("leader runs", func(t *testing.T) {
		svc, provider := setup()
		cache := &fakeLeaderLockCache{}
		svc.SetLeaderLock(cache, nil)

		svc.runScheduledOnce(context.Background())
		require.NotEmpty(t, provider.cleanupTargets)
		require.Equal(t, svc.instanceID, cache.heldBy(batchImageCleanupLeaderLockKey), "锁应持有到周期末尾")
	})
}

type countingCNCheckRepo struct {
	fakeCNCheckRepo
	lists atomic.Int64
}

func (r *countingCNCheckRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	r.lists.Add(1)
	return r.fakeCNCheckRepo.ListByPlatform(ctx, platform)
}

func TestCNProviderBalanceCheck_SkipsWhenPeerHoldsLock(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	repoA := &countingCNCheckRepo{}
	repoB := &countingCNCheckRepo{}
	newSvc := func(repo AccountRepository) *CNProviderBalanceCheckService {
		svc := NewCNProviderBalanceCheckService(repo, nil, nil, &config.Config{}, 10*time.Minute)
		svc.SetLeaderLock(cache, nil)
		return svc
	}
	a, b := newSvc(repoA), newSvc(repoB)

	a.runOnceWithLeaderLock()
	b.runOnceWithLeaderLock()

	require.Positive(t, repoA.lists.Load(), "leader 应执行探测")
	require.Zero(t, repoB.lists.Load(), "同一周期内另一副本必须跳过")
}

type countingScheduledTestPlanRepo struct {
	ScheduledTestPlanRepository
	listDue atomic.Int64
}

func (r *countingScheduledTestPlanRepo) ListDue(context.Context, time.Time) ([]*ScheduledTestPlan, error) {
	r.listDue.Add(1)
	return nil, nil
}

func TestScheduledTestRunner_SkipsWhenPeerHoldsLock(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	repoA := &countingScheduledTestPlanRepo{}
	repoB := &countingScheduledTestPlanRepo{}
	newSvc := func(repo ScheduledTestPlanRepository) *ScheduledTestRunnerService {
		svc := NewScheduledTestRunnerService(repo, nil, nil, nil, nil)
		svc.startupSpread = 0
		svc.SetLeaderLock(cache, nil)
		return svc
	}
	a, b := newSvc(repoA), newSvc(repoB)

	a.runScheduled()
	b.runScheduled()
	a.runScheduled()

	require.EqualValues(t, 1, repoA.listDue.Load(), "同一分钟内 leader 自己的重复 tick 也要跳过（防止上一轮未完成时重扫）")
	require.Zero(t, repoB.listDue.Load(), "同一分钟内另一副本必须跳过")
}

func TestChannelMonitorRunner_SkipsProbeAlreadyDoneByPeer(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	svcA := &stubMonitorSvc{}
	svcB := &stubMonitorSvc{}
	a, b := newRunnerForTest(svcA), newRunnerForTest(svcB)
	a.SetLeaderLock(cache, nil)
	b.SetLeaderLock(cache, nil)
	task := &scheduledMonitor{id: 7, name: "m7", interval: time.Minute}

	require.True(t, a.tryAcquireInFlight(task.id))
	a.runOne(task)
	require.True(t, b.tryAcquireInFlight(task.id))
	b.runOne(task)

	require.EqualValues(t, 1, svcA.runCount.Load())
	require.Zero(t, svcB.runCount.Load(), "同一 monitor 本周期已被其他副本探测，必须跳过")

	// 其他 monitor 的锁互不影响。
	other := &scheduledMonitor{id: 8, name: "m8", interval: time.Minute}
	require.True(t, b.tryAcquireInFlight(other.id))
	b.runOne(other)
	require.EqualValues(t, 1, svcB.runCount.Load())
}

func TestScheduledMonitorMinDelay(t *testing.T) {
	require.Equal(t, 50*time.Second, (&scheduledMonitor{interval: time.Minute, jitter: 10 * time.Second}).minDelay())
	require.Equal(t, monitorMinIntervalSeconds*time.Second, (&scheduledMonitor{interval: 20 * time.Second, jitter: 10 * time.Second}).minDelay())
}

type mutableMonitorSvc struct {
	stubMonitorSvc
	mu      sync.Mutex
	current []*ChannelMonitor
}

func (s *mutableMonitorSvc) ListEnabledMonitors(context.Context) ([]*ChannelMonitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, nil
}

func (s *mutableMonitorSvc) set(ms ...*ChannelMonitor) {
	s.mu.Lock()
	s.current = ms
	s.mu.Unlock()
}

// 多副本下 CRUD 钩子只通知处理请求的那个副本，其余副本靠对账同步任务表。
func TestChannelMonitorRunner_ReconcileSyncsTasksFromDB(t *testing.T) {
	svc := &mutableMonitorSvc{}
	m1 := &ChannelMonitor{ID: 1, Name: "m1", Enabled: true, IntervalSeconds: 60}
	m2 := &ChannelMonitor{ID: 2, Name: "m2", Enabled: true, IntervalSeconds: 60}
	svc.set(m1, m2)
	r := newRunnerForTest(svc)
	r.Start()
	t.Cleanup(r.Stop)
	require.Equal(t, 2, runnerTaskCount(r))
	orig2 := runnerTaskPtr(r, 2)

	// 另一副本上：删除 m1、修改 m2 间隔、新建 m3、m4 解密失败。
	m2changed := &ChannelMonitor{ID: 2, Name: "m2", Enabled: true, IntervalSeconds: 120}
	m3 := &ChannelMonitor{ID: 3, Name: "m3", Enabled: true, IntervalSeconds: 60}
	m4 := &ChannelMonitor{ID: 4, Name: "m4", Enabled: true, IntervalSeconds: 60, APIKeyDecryptFailed: true}
	svc.set(m2changed, m3, m4)
	r.reconcile()

	require.Nil(t, runnerTaskPtr(r, 1), "DB 中已不存在的 monitor 必须取消")
	require.NotNil(t, runnerTaskPtr(r, 3), "新建的 monitor 必须补上任务")
	require.Nil(t, runnerTaskPtr(r, 4), "解密失败的 monitor 不应有任务")
	task2 := runnerTaskPtr(r, 2)
	require.NotSame(t, orig2, task2, "间隔变化必须重建任务")
	require.Equal(t, 120*time.Second, task2.interval)

	// 无变化时不重建。
	r.reconcile()
	require.Same(t, task2, runnerTaskPtr(r, 2))
}

// ---------- 已接锁但每轮即释放的任务：改为持锁到周期末尾 ----------

func TestAccountTTFTMonitor_LeaderLockHeldForCycle(t *testing.T) {
	cache := &fakeLeaderLockCache{}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.TTFTEvalIntervalSeconds = 300
	a := NewAccountTTFTMonitorService(nil, nil, nil, nil, nil, cache, cfg)
	b := NewAccountTTFTMonitorService(nil, nil, nil, nil, nil, cache, cfg)

	release, ok := a.tryAcquireLeaderLock(context.Background())
	require.True(t, ok)
	release()

	_, okB := b.tryAcquireLeaderLock(context.Background())
	require.False(t, okB, "同一周期内另一副本不得再扣分")
	require.GreaterOrEqual(t, cache.ttlOf(accountTTFTMonitorLeaderLockKey), cycleLeaderLockHold(300*time.Second))
}

func newMiniredisClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func TestAccountErrorRateMonitor_LeaderLockHeldForCycle(t *testing.T) {
	mr, rdb := newMiniredisClient(t)
	a := NewAccountErrorRateMonitorService(nil, nil, nil, nil, nil, rdb, nil)
	b := NewAccountErrorRateMonitorService(nil, nil, nil, nil, nil, rdb, nil)
	lock := OpsDistributedLockSettings{Enabled: true, TTLSeconds: 30}

	release, ok := a.tryAcquireLeaderLock(context.Background(), lock, accountErrorRateMonitorInterval)
	require.True(t, ok)
	release()

	_, okB := b.tryAcquireLeaderLock(context.Background(), lock, accountErrorRateMonitorInterval)
	require.False(t, okB, "同一周期内另一副本不得重复告警/剥离")
	require.True(t, mr.Exists(accountErrorRateMonitorLeaderLockKey))
	require.GreaterOrEqual(t, mr.TTL(accountErrorRateMonitorLeaderLockKey), cycleLeaderLockHold(accountErrorRateMonitorInterval),
		"配置的 TTL 小于持有时长时必须抬高，否则锁会提前过期")

	// 模拟周期结束（TTL 到期），另一副本可以接管。
	mr.FastForward(accountErrorRateMonitorInterval)
	releaseB, okB := b.tryAcquireLeaderLock(context.Background(), lock, accountErrorRateMonitorInterval)
	require.True(t, okB)
	releaseB()
}

func TestOpsMetricsCollector_LeaderLockHeldForCycle(t *testing.T) {
	mr, rdb := newMiniredisClient(t)
	a := &OpsMetricsCollector{redisClient: rdb, instanceID: "A"}
	b := &OpsMetricsCollector{redisClient: rdb, instanceID: "B"}
	interval := 5 * time.Minute

	release, ok := a.tryAcquireLeaderLock(context.Background(), interval)
	require.True(t, ok)
	release()

	_, okB := b.tryAcquireLeaderLock(context.Background(), interval)
	require.False(t, okB, "同一周期内另一副本不得再写一行指标")
	require.GreaterOrEqual(t, mr.TTL(opsMetricsCollectorLeaderLockKey), cycleLeaderLockHold(interval))

	mr.FastForward(interval)
	releaseB, okB := b.tryAcquireLeaderLock(context.Background(), interval)
	require.True(t, okB)
	releaseB()
}
