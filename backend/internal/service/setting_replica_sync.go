package service

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SettingsChangeNotifier broadcasts "system settings were saved" to every
// replica sharing the same database. Settings caches are process-local, so
// without it an admin save only takes effect on the replica that served it;
// other replicas converge through their cache TTLs, and the ones without TTL
// (forwarded client IP policy, injected index.html settings, CSP frame-src)
// never converge until restart.
type SettingsChangeNotifier interface {
	PublishSettingsUpdated(ctx context.Context) error
	// SubscribeSettingsUpdates invokes handler for updates published by other
	// replicas. It returns once the subscription is established.
	SubscribeSettingsUpdates(ctx context.Context, handler func()) error
}

const (
	settingsChangePublishTimeout      = 3 * time.Second
	settingsChangeReloadTimeout       = 10 * time.Second
	forwardedClientIPSettingsInterval = time.Minute
)

type settingsReplicaSync struct {
	mu       sync.Mutex
	notifier SettingsChangeNotifier
	cancel   context.CancelFunc
	done     chan struct{}
	reloadMu sync.Mutex
}

// SetSettingsChangeNotifier injects the cross-replica broadcaster.
func (s *SettingService) SetSettingsChangeNotifier(notifier SettingsChangeNotifier) {
	if s == nil {
		return
	}
	s.replicaSync.mu.Lock()
	s.replicaSync.notifier = notifier
	s.replicaSync.mu.Unlock()
}

func (s *SettingService) settingsChangeNotifier() SettingsChangeNotifier {
	if s == nil {
		return nil
	}
	s.replicaSync.mu.Lock()
	defer s.replicaSync.mu.Unlock()
	return s.replicaSync.notifier
}

func (s *SettingService) publishSettingsUpdated(ctx context.Context) {
	notifier := s.settingsChangeNotifier()
	if notifier == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settingsChangePublishTimeout)
	defer cancel()
	if err := notifier.PublishSettingsUpdated(publishCtx); err != nil {
		slog.Warn("publish settings update to other replicas failed", "error", err)
	}
}

// ReloadCachedSettingsFromStore rebuilds every process-local settings cache
// from the database. It runs when another replica saved settings.
func (s *SettingService) ReloadCachedSettingsFromStore(ctx context.Context) error {
	if s == nil || s.settingRepo == nil {
		return nil
	}
	s.replicaSync.reloadMu.Lock()
	defer s.replicaSync.reloadMu.Unlock()
	stored, err := s.GetAllSettings(ctx)
	if err != nil {
		return err
	}
	s.refreshCachedSettings(stored)
	return nil
}

// RefreshForwardedClientIPSettings re-reads the forwarded client IP policy
// without the one-time migration performed at startup. A read failure keeps
// the last known policy instead of failing closed on a transient DB error.
func (s *SettingService) RefreshForwardedClientIPSettings(ctx context.Context) error {
	if s == nil || s.cfg == nil || s.settingRepo == nil {
		return nil
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyAPIKeyACLTrustForwardedIP,
		SettingKeyForwardedClientIPHeaders,
	})
	if err != nil {
		return err
	}
	current := s.cfg.ForwardedClientIPSettings()
	enabled := s.cfg.Security.TrustForwardedIPForAPIKeyACL
	if stored, ok := values[SettingKeyAPIKeyACLTrustForwardedIP]; ok {
		enabled = stored == "true"
	}
	headers := current.Headers
	var headersErr error
	if stored, ok := values[SettingKeyForwardedClientIPHeaders]; ok {
		headers, headersErr = parseForwardedClientIPHeadersSetting(stored)
		if headersErr != nil {
			enabled = false
			headers = []string{}
		}
	}
	s.cfg.SetForwardedClientIPSettings(enabled, headers)
	return headersErr
}

// StartReplicaSettingsSync subscribes to settings broadcasts from other
// replicas and periodically re-reads the forwarded client IP policy as a
// fallback for missed broadcasts (pub/sub is fire-and-forget).
func (s *SettingService) StartReplicaSettingsSync(ctx context.Context) {
	s.startReplicaSettingsSync(ctx, forwardedClientIPSettingsInterval)
}

func (s *SettingService) startReplicaSettingsSync(ctx context.Context, interval time.Duration) {
	if s == nil || s.settingRepo == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.replicaSync.mu.Lock()
	if s.replicaSync.cancel != nil {
		s.replicaSync.mu.Unlock()
		return
	}
	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.replicaSync.cancel = cancel
	s.replicaSync.done = done
	notifier := s.replicaSync.notifier
	s.replicaSync.mu.Unlock()

	if notifier != nil {
		if err := notifier.SubscribeSettingsUpdates(syncCtx, func() {
			reloadCtx, reloadCancel := context.WithTimeout(syncCtx, settingsChangeReloadTimeout)
			defer reloadCancel()
			if err := s.ReloadCachedSettingsFromStore(reloadCtx); err != nil {
				slog.Warn("reload settings after update from another replica failed", "error", err)
			}
		}); err != nil {
			slog.Warn("subscribe settings updates failed; relying on periodic refresh", "error", err)
		}
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-syncCtx.Done():
				return
			case <-ticker.C:
				refreshCtx, refreshCancel := context.WithTimeout(syncCtx, settingsChangeReloadTimeout)
				if err := s.RefreshForwardedClientIPSettings(refreshCtx); err != nil {
					slog.Warn("periodic forwarded client IP settings refresh failed", "error", err)
				}
				refreshCancel()
			}
		}
	}()
}

// Stop ends the replica settings sync loop.
func (s *SettingService) Stop() {
	if s == nil {
		return
	}
	s.replicaSync.mu.Lock()
	cancel := s.replicaSync.cancel
	done := s.replicaSync.done
	s.replicaSync.cancel = nil
	s.replicaSync.done = nil
	s.replicaSync.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
