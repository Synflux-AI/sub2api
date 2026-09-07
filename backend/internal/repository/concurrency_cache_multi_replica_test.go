package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// TestStartupCleanupSurvivesRollingRestart 模拟 start-first 滚动发布与双副本：
// 副本 A 持有槽位与等待计数期间，副本 B 反复启动（每次都跑启动清理），
// A 的读数不得异常归零，并发上限也不得被突破。
func TestStartupCleanupSurvivesRollingRestart(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	ctx := context.Background()

	// 两个 cache 实例代表两个进程：它们共享 Redis，但 request ID 各自独立。
	replicaA := NewConcurrencyCache(client, 15, 900)
	replicaB := NewConcurrencyCache(client, 15, 900)

	const (
		accountID      = int64(7101)
		userID         = int64(7102)
		maxConcurrency = 3
	)

	require.NoError(t, replicaA.CleanupStaleProcessSlots(ctx))

	for i := 0; i < maxConcurrency; i++ {
		requestID := fmt.Sprintf("procA-%d", i)
		ok, err := replicaA.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = replicaA.AcquireUserSlot(ctx, userID, maxConcurrency, requestID)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := replicaA.IncrementAccountWaitCount(ctx, accountID, 10)
	require.NoError(t, err)
	require.True(t, ok)

	// 反复重启 B：每次启动清理都不得动到 A 仍在服务的状态。
	for round := 0; round < 5; round++ {
		require.NoError(t, replicaB.CleanupStaleProcessSlots(ctx))

		counts, err := replicaB.GetAccountConcurrencyBatch(ctx, []int64{accountID})
		require.NoError(t, err)
		require.Equal(t, maxConcurrency, counts[accountID],
			"round %d: account slots held by replica A must not be dropped", round)

		userCount, err := replicaB.GetUserConcurrency(ctx, userID)
		require.NoError(t, err)
		require.Equal(t, maxConcurrency, userCount, "round %d: user slots must not be dropped", round)

		waiting, err := replicaB.GetAccountWaitingCount(ctx, accountID)
		require.NoError(t, err)
		require.Equal(t, 1, waiting, "round %d: shared wait counter must not be deleted", round)

		acquired, err := replicaB.AcquireAccountSlot(ctx, accountID, maxConcurrency, fmt.Sprintf("procB-%d", round))
		require.NoError(t, err)
		require.False(t, acquired, "round %d: account concurrency cap must stay enforced", round)
		acquired, err = replicaB.AcquireUserSlot(ctx, userID, maxConcurrency, fmt.Sprintf("procB-%d", round))
		require.NoError(t, err)
		require.False(t, acquired, "round %d: user concurrency cap must stay enforced", round)
	}

	// A 正常释放后容量重新可用，说明启动清理没有留下幽灵槽位。
	require.NoError(t, replicaA.ReleaseAccountSlot(ctx, accountID, "procA-0"))
	acquired, err := replicaB.AcquireAccountSlot(ctx, accountID, maxConcurrency, "procB-final")
	require.NoError(t, err)
	require.True(t, acquired)
}
