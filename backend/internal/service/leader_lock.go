package service

import (
	"context"
	"database/sql"
	"time"
)

// LeaderLockCache provides cross-instance mutual exclusion for periodic background
// jobs. It is implemented in the repository layer (Redis-backed) so the service
// layer never depends on Redis directly. Release is a compare-and-delete keyed by
// owner so a stale holder can never delete a peer's lock.
type LeaderLockCache interface {
	// TryAcquireLeaderLock sets key=owner with the given TTL iff key is absent.
	// It returns true when the caller becomes the owner.
	TryAcquireLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
	// ReleaseLeaderLock deletes key iff it is still owned by owner.
	ReleaseLeaderLock(ctx context.Context, key, owner string) error
}

// tryAcquireSingletonLeaderLock provides best-effort single-flight execution of a
// periodic background job across multiple instances. It prefers the Redis-backed
// LeaderLockCache and falls back to a Postgres advisory lock when the cache is
// unavailable or errors, mirroring the approach used by the Ops background
// services.
//
// Semantics:
//   - acquired      -> returns a non-nil release func and true; callers should
//     defer the release once the job finishes.
//   - held by peer  -> returns (nil, false); callers should skip this cycle.
//   - no backend    -> when neither the cache nor a DB is configured (e.g. unit
//     tests, or a single-instance deployment without Redis) it runs without
//     gating, returning a no-op release and true, so the job is never silently
//     starved.
//
// The TTL is purely a crash-safety bound: callers release the lock as soon as the
// job completes, so leadership is re-contested every cycle rather than pinned to
// one instance. The TTL must therefore be larger than the job's worst-case
// runtime so the lock does not expire mid-run.
func tryAcquireSingletonLeaderLock(ctx context.Context, cache LeaderLockCache, db *sql.DB, key, owner string, ttl time.Duration) (func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}

	if cache != nil {
		ok, err := cache.TryAcquireLeaderLock(ctx, key, owner, ttl)
		if err == nil {
			if !ok {
				return nil, false
			}
			release := func() {
				ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = cache.ReleaseLeaderLock(ctx2, key, owner)
			}
			return release, true
		}
		// Cache error: fall through to the DB advisory lock so a flaky Redis does
		// not stampede the job across every instance.
	}

	if db != nil {
		return tryAcquireDBAdvisoryLock(ctx, db, hashAdvisoryLockID(key))
	}

	// No coordination backend available: run without gating.
	return func() {}, true
}

// cycleLeaderLockHold 返回「每周期只执行一次」语义下锁的最短持有时长：取周期的 90%。
// 略短于周期是为了让同一实例下一次 tick 一定能重新抢到锁（单副本绝不因此漏跑），
// 同时保证多副本下相邻两次执行的间隔不小于 0.9 个周期。
func cycleLeaderLockHold(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return interval * 9 / 10
}

// releaseLeaderLockAfter 把锁的释放推迟到 releaseAt。已过期则立即释放。
//
// 背景：多数周期任务在每轮结束时立刻释放 leader lock，N 个副本各自的计时器相位错开，
// 锁一释放下一个副本就能拿到 → 每个周期执行 N 次（只是互不重叠）。把锁持有到周期
// 末尾才能真正做到「全局每周期一次」。做法同 upstream_billing_probe。
func releaseLeaderLockAfter(release func(), releaseAt time.Time) {
	if release == nil {
		return
	}
	delay := time.Until(releaseAt)
	if delay <= 0 {
		release()
		return
	}
	time.AfterFunc(delay, release)
}

// tryAcquireCycleLeaderLock 是 tryAcquireSingletonLeaderLock 的「每周期一次」版本：
//
//   - Redis 路径：抢到锁后，调用方在本轮结束时调用返回的 release，实际释放会推迟到
//     「抢锁时刻 + hold」，因此其他副本在本周期内的 tick 都会跳过；本轮若跑得比 hold
//     还久，则在结束时立即释放，全程不会与其他副本重叠。
//   - DB advisory 回退路径：release 立即生效，不为持锁长期占用连接池里的一条连接，
//     语义退化为「互不重叠」。只在 Redis 故障时出现，可接受。
//   - 无任何协调后端：不设闸门直接执行（单实例/单测）。
//
// ttl 是崩溃兜底：持有者崩溃后最迟 ttl 到期被其他副本接管；它必须大于单轮最坏执行
// 时长，且会被自动抬到不小于 hold。
func tryAcquireCycleLeaderLock(ctx context.Context, cache LeaderLockCache, db *sql.DB, key, owner string, hold, ttl time.Duration) (func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ttl < hold {
		ttl = hold
	}

	if cache != nil {
		acquiredAt := time.Now()
		ok, err := cache.TryAcquireLeaderLock(ctx, key, owner, ttl)
		if err == nil {
			if !ok {
				return nil, false
			}
			release := func() {
				ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = cache.ReleaseLeaderLock(ctx2, key, owner)
			}
			return func() { releaseLeaderLockAfter(release, acquiredAt.Add(hold)) }, true
		}
		// Redis 报错：回退到 DB advisory lock，避免 Redis 抖动时所有副本同时执行。
	}

	if db != nil {
		return tryAcquireDBAdvisoryLock(ctx, db, hashAdvisoryLockID(key))
	}

	return func() {}, true
}
