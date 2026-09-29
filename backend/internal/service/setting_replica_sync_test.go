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
	"github.com/stretchr/testify/require"
)

// sharedSettingRepo is one settings table shared by several replicas.
type sharedSettingRepo struct {
	mu        sync.Mutex
	values    map[string]string
	failReads bool
}

func newSharedSettingRepo() *sharedSettingRepo {
	return &sharedSettingRepo{values: map[string]string{}}
}

func (r *sharedSettingRepo) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get call")
}

func (r *sharedSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *sharedSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}

func (r *sharedSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failReads {
		return nil, errors.New("db down")
	}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

func (r *sharedSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range values {
		r.values[k] = v
	}
	return nil
}

func (r *sharedSettingRepo) GetAll(context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failReads {
		return nil, errors.New("db down")
	}
	result := make(map[string]string, len(r.values))
	for k, v := range r.values {
		result[k] = v
	}
	return result, nil
}

func (r *sharedSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

// fakeSettingsBus delivers publishes synchronously to every other subscriber.
type fakeSettingsBus struct {
	mu   sync.Mutex
	subs map[string]func()
}

type fakeSettingsNotifier struct {
	bus *fakeSettingsBus
	id  string
}

func (b *fakeSettingsBus) notifier(id string) *fakeSettingsNotifier {
	return &fakeSettingsNotifier{bus: b, id: id}
}

func (n *fakeSettingsNotifier) PublishSettingsUpdated(context.Context) error {
	n.bus.mu.Lock()
	handlers := make([]func(), 0, len(n.bus.subs))
	for id, h := range n.bus.subs {
		if id != n.id {
			handlers = append(handlers, h)
		}
	}
	n.bus.mu.Unlock()
	for _, h := range handlers {
		h()
	}
	return nil
}

func (n *fakeSettingsNotifier) SubscribeSettingsUpdates(_ context.Context, handler func()) error {
	n.bus.mu.Lock()
	defer n.bus.mu.Unlock()
	if n.bus.subs == nil {
		n.bus.subs = map[string]func(){}
	}
	n.bus.subs[n.id] = handler
	return nil
}

func TestSettingsSaveOnOneReplicaReloadsOtherReplicas(t *testing.T) {
	repo := newSharedSettingRepo()
	bus := &fakeSettingsBus{}

	cfgA, cfgB := &config.Config{}, &config.Config{}
	replicaA := NewSettingService(repo, cfgA)
	replicaB := NewSettingService(repo, cfgB)
	replicaA.SetSettingsChangeNotifier(bus.notifier("a"))
	replicaB.SetSettingsChangeNotifier(bus.notifier("b"))
	var updatesA, updatesB atomic.Int32
	replicaA.SetOnUpdateCallback(func() { updatesA.Add(1) })
	replicaB.SetOnUpdateCallback(func() { updatesB.Add(1) })
	replicaA.startReplicaSettingsSync(context.Background(), time.Hour)
	replicaB.startReplicaSettingsSync(context.Background(), time.Hour)
	t.Cleanup(func() {
		replicaA.Stop()
		replicaB.Stop()
		replicaA.refreshCachedSettings(&SystemSettings{})
	})

	// Replica A persists and applies the new forwarded-IP policy, then broadcasts.
	require.NoError(t, repo.SetMultiple(context.Background(), map[string]string{
		SettingKeyAPIKeyACLTrustForwardedIP: "true",
		SettingKeyForwardedClientIPHeaders:  `["CF-Connecting-IP"]`,
	}))
	replicaA.refreshCachedSettingsAfterWrite(context.Background(), &SystemSettings{
		APIKeyACLTrustForwardedIP: true,
		ForwardedClientIPHeaders:  []string{"CF-Connecting-IP"},
	}, nil)

	require.True(t, cfgA.TrustForwardedIPForAPIKeyACL())
	require.True(t, cfgB.TrustForwardedIPForAPIKeyACL(), "replica B must apply the policy saved on replica A")
	require.Equal(t, []string{"Cf-Connecting-Ip"}, cfgB.ForwardedClientIPSettings().Headers)
	require.Equal(t, int32(1), updatesA.Load())
	require.Equal(t, int32(1), updatesB.Load(), "replica B must invalidate its index.html / CSP caches too")
}

func TestRefreshForwardedClientIPSettingsKeepsLastKnownOnReadError(t *testing.T) {
	repo := newSharedSettingRepo()
	cfg := &config.Config{}
	svc := NewSettingService(repo, cfg)

	require.NoError(t, repo.SetMultiple(context.Background(), map[string]string{
		SettingKeyAPIKeyACLTrustForwardedIP: "true",
		SettingKeyForwardedClientIPHeaders:  `["X-Real-IP"]`,
	}))
	require.NoError(t, svc.RefreshForwardedClientIPSettings(context.Background()))
	require.True(t, cfg.TrustForwardedIPForAPIKeyACL())
	require.Equal(t, []string{"X-Real-Ip"}, cfg.ForwardedClientIPSettings().Headers)

	repo.mu.Lock()
	repo.failReads = true
	repo.mu.Unlock()
	require.Error(t, svc.RefreshForwardedClientIPSettings(context.Background()))
	require.True(t, cfg.TrustForwardedIPForAPIKeyACL(), "a transient DB error must not flip the policy")
	require.Equal(t, []string{"X-Real-Ip"}, cfg.ForwardedClientIPSettings().Headers)

	repo.mu.Lock()
	repo.failReads = false
	repo.values[SettingKeyForwardedClientIPHeaders] = `{"bad":true}`
	repo.mu.Unlock()
	require.Error(t, svc.RefreshForwardedClientIPSettings(context.Background()))
	require.False(t, cfg.TrustForwardedIPForAPIKeyACL(), "malformed stored headers fail closed, same as startup")
}

func TestReplicaSettingsSyncPeriodicallyRefreshesForwardedIP(t *testing.T) {
	repo := newSharedSettingRepo()
	cfg := &config.Config{}
	svc := NewSettingService(repo, cfg)
	svc.startReplicaSettingsSync(context.Background(), 10*time.Millisecond)
	t.Cleanup(svc.Stop)

	require.NoError(t, repo.SetMultiple(context.Background(), map[string]string{
		SettingKeyAPIKeyACLTrustForwardedIP: "true",
	}))
	require.Eventually(t, cfg.TrustForwardedIPForAPIKeyACL, time.Second, 5*time.Millisecond,
		"without a broadcast the periodic refresh must still converge")

	svc.Stop()
	svc.Stop() // idempotent
}
