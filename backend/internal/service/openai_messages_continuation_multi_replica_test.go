//go:build unit

package service

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// sharedCompatSessionStateCache simulates the Redis-backed gateway cache that
// every replica shares. Only the compat session state capability is implemented.
type sharedCompatSessionStateCache struct {
	GatewayCache

	mu      sync.Mutex
	entries map[string][]byte
	fail    bool
}

func newSharedCompatSessionStateCache() *sharedCompatSessionStateCache {
	return &sharedCompatSessionStateCache{entries: make(map[string][]byte)}
}

func (c *sharedCompatSessionStateCache) GetOpenAICompatSessionState(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return nil, errors.New("redis down")
	}
	return c.entries[key], nil
}

func (c *sharedCompatSessionStateCache) SetOpenAICompatSessionState(_ context.Context, key string, payload []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("redis down")
	}
	if ttl <= 0 {
		return errors.New("ttl required")
	}
	c.entries[key] = append([]byte(nil), payload...)
	return nil
}

func (c *sharedCompatSessionStateCache) DeleteOpenAICompatSessionState(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("redis down")
	}
	delete(c.entries, key)
	return nil
}

func TestOpenAICompatSessionStateSharedAcrossReplicas(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := newSharedCompatSessionStateCache()
	replicaA := &OpenAIGatewayService{cache: cache}
	replicaB := &OpenAIGatewayService{cache: cache}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.Background()
	const promptCacheKey = "pck-1"

	replicaA.bindOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey, "turn-a")
	replicaA.bindOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey, "resp_1")
	require.Equal(t, "resp_1", replicaB.getOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey))
	require.Equal(t, "turn-a", replicaB.getOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey))

	// Turn two lands on replica B; replica A must not resume from its own stale id.
	replicaB.bindOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey, "resp_2")
	require.Equal(t, "resp_2", replicaA.getOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey))
	require.Equal(t, "turn-a", replicaA.getOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey))

	replicaA.deleteOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey)
	require.Empty(t, replicaB.getOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey))
	require.Equal(t, "turn-a", replicaB.getOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey))

	replicaB.disableOpenAICompatSessionContinuation(ctx, c, account, promptCacheKey)
	require.True(t, replicaA.isOpenAICompatSessionContinuationDisabled(ctx, c, account, promptCacheKey))
	replicaA.bindOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey, "resp_3")
	require.Empty(t, replicaB.getOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey),
		"a session disabled on one replica must stay disabled on the others")

	_, hasLocal := replicaA.openaiCompatSessionResponses.Load(openAICompatSessionResponseKey(c, account, promptCacheKey))
	require.False(t, hasLocal, "the shared cache is the source of truth; no stale local copy")
}

func TestOpenAICompatSessionStateExpiresInSharedCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := newSharedCompatSessionStateCache()
	svc := &OpenAIGatewayService{cache: cache}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.Background()
	key := openAICompatSessionResponseKey(c, account, "pck-exp")

	svc.storeOpenAICompatSessionBinding(ctx, key, openAICompatSessionResponseBinding{
		ResponseID: "resp_old",
		ExpiresAt:  time.Now().Add(-time.Second),
	})
	require.Empty(t, svc.getOpenAICompatSessionResponseID(ctx, c, account, "pck-exp"))
}

func TestOpenAICompatSessionStateFallsBackToLocalWhenSharedCacheFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := newSharedCompatSessionStateCache()
	cache.fail = true
	svc := &OpenAIGatewayService{cache: cache}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.Background()

	svc.bindOpenAICompatSessionResponseID(ctx, c, account, "pck-2", "resp_local")
	require.Equal(t, "resp_local", svc.getOpenAICompatSessionResponseID(ctx, c, account, "pck-2"))
}

func TestOpenAICompatSessionStateWithoutSharedCacheIsProcessLocal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.Background()

	svc.bindOpenAICompatSessionResponseID(ctx, c, account, "pck-3", "resp_mem")
	require.Equal(t, "resp_mem", svc.getOpenAICompatSessionResponseID(ctx, c, account, "pck-3"))
	svc.disableOpenAICompatSessionContinuation(ctx, c, account, "pck-3")
	require.True(t, svc.isOpenAICompatSessionContinuationDisabled(ctx, c, account, "pck-3"))
	require.Empty(t, svc.getOpenAICompatSessionResponseID(ctx, c, account, "pck-3"))
}
