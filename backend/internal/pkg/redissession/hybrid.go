package redissession

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Hybrid stores OAuth sessions in Redis so that the "generate auth URL" and
// "exchange code" requests may land on different replicas. A process-local
// copy is kept only as a fallback for sessions whose Redis write failed; when
// Redis is not configured it behaves exactly like an in-memory map.
type Hybrid[T any] struct {
	name      string
	ttl       time.Duration
	createdAt func(*T) time.Time

	mu        sync.RWMutex
	local     map[string]*T
	localOnly map[string]struct{}
	remote    *Store
}

// NewHybrid creates a session store. rdb may be nil (process-local only).
// createdAt returns the session creation time used for TTL checks.
func NewHybrid[T any](rdb *redis.Client, name string, ttl time.Duration, createdAt func(*T) time.Time) *Hybrid[T] {
	h := &Hybrid[T]{
		name:      name,
		ttl:       ttl,
		createdAt: createdAt,
		local:     make(map[string]*T),
		localOnly: make(map[string]struct{}),
	}
	if rdb != nil {
		h.remote = New(rdb, "oauth:session:"+name, ttl)
	}
	return h
}

// Distributed reports whether sessions are shared through Redis.
func (h *Hybrid[T]) Distributed() bool {
	return h != nil && h.remote != nil
}

func (h *Hybrid[T]) expired(session *T) bool {
	if session == nil {
		return true
	}
	if h.createdAt == nil || h.ttl <= 0 {
		return false
	}
	return time.Since(h.createdAt(session)) > h.ttl
}

func (h *Hybrid[T]) Set(sessionID string, session *T) {
	if h == nil || session == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	var remoteErr error
	if h.remote != nil {
		remoteErr = h.remote.Set(context.Background(), sessionID, session)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.remote == nil {
		h.local[sessionID] = session
		return
	}
	if remoteErr != nil {
		h.local[sessionID] = session
		h.localOnly[sessionID] = struct{}{}
		slog.Warn("oauth session Redis write failed; using process-local fallback", "store", h.name, "error", remoteErr)
		return
	}
	delete(h.local, sessionID)
	delete(h.localOnly, sessionID)
}

func (h *Hybrid[T]) Get(sessionID string) (*T, bool) {
	if h == nil {
		return nil, false
	}
	sessionID = strings.TrimSpace(sessionID)
	if h.remote == nil || h.isLocalOnly(sessionID) {
		return h.getLocal(sessionID)
	}
	session := new(T)
	ok, err := h.remote.Get(context.Background(), sessionID, session)
	if err != nil {
		slog.Warn("oauth session Redis read failed", "store", h.name, "error", err)
		return nil, false
	}
	if !ok || h.expired(session) {
		return nil, false
	}
	return session, true
}

func (h *Hybrid[T]) getLocal(sessionID string) (*T, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	session, ok := h.local[sessionID]
	if !ok || h.expired(session) {
		return nil, false
	}
	return session, true
}

func (h *Hybrid[T]) isLocalOnly(sessionID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.localOnly[sessionID]
	return ok
}

func (h *Hybrid[T]) Delete(sessionID string) {
	if h == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if h.remote != nil {
		if err := h.remote.Delete(context.Background(), sessionID); err != nil {
			slog.Warn("oauth session Redis delete failed", "store", h.name, "error", err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.local, sessionID)
	delete(h.localOnly, sessionID)
}

// CleanupExpired drops expired process-local copies. Redis entries expire by TTL.
func (h *Hybrid[T]) CleanupExpired() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, session := range h.local {
		if h.expired(session) {
			delete(h.local, id)
			delete(h.localOnly, id)
		}
	}
}
