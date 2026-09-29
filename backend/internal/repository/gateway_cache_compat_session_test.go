package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGatewayCacheOpenAICompatSessionState(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cache, ok := NewGatewayCache(client).(*gatewayCache)
	require.True(t, ok)
	ctx := context.Background()

	got, err := cache.GetOpenAICompatSessionState(ctx, "k1")
	require.NoError(t, err)
	require.Nil(t, got, "miss returns nil without error")

	require.NoError(t, cache.SetOpenAICompatSessionState(ctx, "k1", []byte(`{"response_id":"resp_1"}`), time.Minute))
	got, err = cache.GetOpenAICompatSessionState(ctx, "k1")
	require.NoError(t, err)
	require.JSONEq(t, `{"response_id":"resp_1"}`, string(got))
	require.InDelta(t, time.Minute.Seconds(), mr.TTL(openAICompatSessionStatePrefix+"k1").Seconds(), 1)

	require.NoError(t, cache.DeleteOpenAICompatSessionState(ctx, "k1"))
	got, err = cache.GetOpenAICompatSessionState(ctx, "k1")
	require.NoError(t, err)
	require.Nil(t, got)

	require.Error(t, cache.SetOpenAICompatSessionState(ctx, "", []byte("x"), time.Minute))
	require.Error(t, cache.SetOpenAICompatSessionState(ctx, "k2", nil, time.Minute))
}
