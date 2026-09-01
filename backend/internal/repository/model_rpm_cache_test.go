package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newTestModelRPMCache(t *testing.T) (*miniredis.Miniredis, *modelRPMCacheImpl) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, &modelRPMCacheImpl{rdb: rdb}
}

func TestModelRPMCacheUserScopeKeyFormatAndTTL(t *testing.T) {
	mr, cache := newTestModelRPMCache(t)
	ctx := context.Background()

	count, err := cache.IncrementRuleRPM(ctx, 7, 42, 29000000)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	const key = "rpm:mu:7:42:29000000"
	stored, err := mr.Get(key)
	require.NoError(t, err)
	require.Equal(t, "1", stored)
	require.InDelta(t, float64(120*time.Second), float64(mr.TTL(key)), float64(time.Second))

	count, err = cache.IncrementRuleRPM(ctx, 7, 42, 29000000)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestModelRPMCacheGlobalScopeUsesSharedKey(t *testing.T) {
	mr, cache := newTestModelRPMCache(t)
	ctx := context.Background()

	// userID <= 0 均落到同一个全局桶。
	for _, userID := range []int64{0, -1} {
		_, err := cache.IncrementRuleRPM(ctx, 3, userID, 29000001)
		require.NoError(t, err)
	}

	const key = "rpm:mg:3:29000001"
	stored, err := mr.Get(key)
	require.NoError(t, err)
	require.Equal(t, "2", stored)
	require.InDelta(t, float64(120*time.Second), float64(mr.TTL(key)), float64(time.Second))
}

func TestModelRPMCacheBucketsAreIsolatedPerRuleUserAndMinute(t *testing.T) {
	_, cache := newTestModelRPMCache(t)
	ctx := context.Background()

	_, err := cache.IncrementRuleRPM(ctx, 1, 1, 100)
	require.NoError(t, err)

	// 换规则 / 换用户 / 换分钟都必须是新桶。
	for _, args := range [][3]int64{{2, 1, 100}, {1, 2, 100}, {1, 1, 101}} {
		count, incErr := cache.IncrementRuleRPM(ctx, args[0], args[1], args[2])
		require.NoError(t, incErr)
		require.Equal(t, 1, count)
	}
}

func TestModelRPMCacheGetReturnsZeroForMissingKey(t *testing.T) {
	_, cache := newTestModelRPMCache(t)
	ctx := context.Background()

	count, err := cache.GetRuleRPM(ctx, 9, 9, 29000002)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	_, err = cache.IncrementRuleRPM(ctx, 9, 9, 29000002)
	require.NoError(t, err)

	count, err = cache.GetRuleRPM(ctx, 9, 9, 29000002)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestModelRPMCacheMinuteTimestampUsesServerTime(t *testing.T) {
	mr, cache := newTestModelRPMCache(t)
	mr.SetTime(time.Unix(1800000060, 0))

	minute, err := cache.MinuteTimestamp(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1800000060/60, minute)
}
