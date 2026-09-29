//go:build unit

package redissession

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type hybridTestSession struct {
	State        string    `json:"state"`
	CodeVerifier string    `json:"code_verifier"`
	CreatedAt    time.Time `json:"created_at"`
}

func hybridTestCreatedAt(s *hybridTestSession) time.Time { return s.CreatedAt }

func TestHybridSharesSessionsAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	rdbA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdbB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdbA.Close(); _ = rdbB.Close() })

	replicaA := NewHybrid(rdbA, "test", time.Minute, hybridTestCreatedAt)
	replicaB := NewHybrid(rdbB, "test", time.Minute, hybridTestCreatedAt)
	require.True(t, replicaA.Distributed())

	replicaA.Set("sid", &hybridTestSession{State: "st", CodeVerifier: "verifier", CreatedAt: time.Now()})

	got, ok := replicaB.Get("sid")
	require.True(t, ok)
	require.Equal(t, "verifier", got.CodeVerifier)
	require.Equal(t, "st", got.State)

	replicaB.Delete("sid")
	_, ok = replicaA.Get("sid")
	require.False(t, ok, "deleting on one replica must invalidate the session everywhere")
}

func TestHybridRejectsExpiredSession(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := NewHybrid(rdb, "test", time.Minute, hybridTestCreatedAt)
	store.Set("old", &hybridTestSession{State: "st", CreatedAt: time.Now().Add(-2 * time.Minute)})
	_, ok := store.Get("old")
	require.False(t, ok)
}

func TestHybridFallsBackToLocalWhenRedisWriteFails(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })

	store := NewHybrid(rdb, "test", time.Minute, hybridTestCreatedAt)
	mr.Close()

	store.Set("sid", &hybridTestSession{State: "st", CreatedAt: time.Now()})
	got, ok := store.Get("sid")
	require.True(t, ok, "session written during a Redis outage must stay usable on the same replica")
	require.Equal(t, "st", got.State)

	store.Delete("sid")
	_, ok = store.Get("sid")
	require.False(t, ok)
}

func TestHybridWithoutRedisIsProcessLocal(t *testing.T) {
	store := NewHybrid[hybridTestSession](nil, "test", time.Minute, hybridTestCreatedAt)
	require.False(t, store.Distributed())

	store.Set("sid", &hybridTestSession{State: "st", CreatedAt: time.Now()})
	got, ok := store.Get("sid")
	require.True(t, ok)
	require.Equal(t, "st", got.State)

	store.Set("old", &hybridTestSession{State: "old", CreatedAt: time.Now().Add(-2 * time.Minute)})
	store.CleanupExpired()
	_, ok = store.getLocal("old")
	require.False(t, ok)
	_, ok = store.Get("sid")
	require.True(t, ok)
}
