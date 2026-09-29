package repository

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSettingsChangeNotifierSkipsOwnPublishes(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	replicaA := NewSettingsChangeNotifier(rdb)
	replicaB := NewSettingsChangeNotifier(rdb)
	var gotA, gotB atomic.Int32
	require.NoError(t, replicaA.SubscribeSettingsUpdates(ctx, func() { gotA.Add(1) }))
	require.NoError(t, replicaB.SubscribeSettingsUpdates(ctx, func() { gotB.Add(1) }))

	require.NoError(t, replicaA.PublishSettingsUpdated(ctx))
	require.Eventually(t, func() bool { return gotB.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(0), gotA.Load(), "a replica must not reload on its own save")
}

func TestSettingsChangeNotifierNilRedis(t *testing.T) {
	require.Nil(t, NewSettingsChangeNotifier(nil))
}
