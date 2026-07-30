package repository

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newTestAccountHealthCache(t *testing.T) (*miniredis.Miniredis, *accountHealthCache) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, &accountHealthCache{rdb: rdb}
}

func TestAccountHealthCacheApplyDeltaPenaltyAndClamp(t *testing.T) {
	_, cache := newTestAccountHealthCache(t)
	ctx := context.Background()

	score, err := cache.ApplyDelta(ctx, 1, -35, 600, 1800)
	require.NoError(t, err)
	require.InDelta(t, 65, score, 0.001)

	// 连续扣分 clamp 到 0
	for i := 0; i < 5; i++ {
		score, err = cache.ApplyDelta(ctx, 1, -35, 600, 1800)
		require.NoError(t, err)
	}
	require.Equal(t, 0.0, score)
}

func TestAccountHealthCacheApplyDeltaRecoverDeletesKey(t *testing.T) {
	mr, cache := newTestAccountHealthCache(t)
	ctx := context.Background()

	_, err := cache.ApplyDelta(ctx, 2, -3, 600, 1800)
	require.NoError(t, err)
	require.True(t, mr.Exists(accountHealthKey(2)))

	// 成功加分回到 100 后 key 删除（完全恢复零常驻）
	score, err := cache.ApplyDelta(ctx, 2, 5, 600, 1800)
	require.NoError(t, err)
	require.Equal(t, 100.0, score)
	require.False(t, mr.Exists(accountHealthKey(2)))
}

func TestAccountHealthCacheApplyDeltaDecaysBetweenWrites(t *testing.T) {
	mr, cache := newTestAccountHealthCache(t)
	ctx := context.Background()

	_, err := cache.ApplyDelta(ctx, 3, -40, 600, 1800)
	require.NoError(t, err)

	// 手动把 ts 拨回一个半衰期前：60 分应衰减为 80 分，再扣 10 => 70
	mr.HSet(accountHealthKey(3), "ts", "0")
	entries, err := cache.GetScoresBatch(ctx, []int64{3})
	require.NoError(t, err)
	require.InDelta(t, 60, entries[3].Score, 0.001)

	score, err := cache.ApplyDelta(ctx, 3, -10, 600, 1800)
	require.NoError(t, err)
	// elapsed >> halflife，60 分几乎完全衰减回 100，再扣 10
	require.InDelta(t, 90, score, 0.5)
}

func TestAccountHealthCacheGetScoresBatch(t *testing.T) {
	_, cache := newTestAccountHealthCache(t)
	ctx := context.Background()

	_, err := cache.ApplyDelta(ctx, 10, -20, 600, 1800)
	require.NoError(t, err)
	_, err = cache.ApplyDelta(ctx, 11, -50, 600, 1800)
	require.NoError(t, err)

	entries, err := cache.GetScoresBatch(ctx, []int64{10, 11, 12})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.InDelta(t, 80, entries[10].Score, 0.001)
	require.InDelta(t, 50, entries[11].Score, 0.001)
	require.NotContains(t, entries, int64(12)) // 无记录 = 满分，不出现在结果中
	require.False(t, entries[10].UpdatedAt.IsZero())
}

func TestAccountHealthCacheGetScoresBatchEmpty(t *testing.T) {
	_, cache := newTestAccountHealthCache(t)
	entries, err := cache.GetScoresBatch(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, entries)
}
