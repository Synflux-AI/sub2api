package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/dgraph-io/ristretto"
)

// v17: 双方再次撞号——本仓库 v16(group web search per-call pricing)与上游 v16
// (group reasoning effort ceiling + mappings)字段集不同,合并后快照与任一方 v16
// 均不兼容,升 17 强制失效。
// v18 补齐余额通知收件人判定依赖的 signup_source,避免邮箱注册用户被还原为空来源。
// v19: 又一次撞号——本仓库 v18(signup_source)与上游 v17(group Live gate)字段集不同,
// 合并后快照同时含 signup_source 与 allow_live,与任一方旧版本均不兼容,升 19 强制失效。
// v20: 再次撞号——上游 v18(group profit control fields)与本仓库 v19 字段集不同,
// 合并后快照同时含 profit control 字段与本仓库扩展字段,与任一方旧版本均不兼容,升 20 强制失效。
// v21: 又一次撞号——上游 v19(group search/audio/video_model_prices 计费字段)与本仓库 v20
// 字段集不同,合并后快照同时含上游计费字段与本仓库 codex_cli_only 等扩展字段,
// 与任一方旧版本均不兼容,升 21 强制失效。
// v22: 再次撞号——上游 v20(group long-context and model pricing fields)与本仓库 v21
// 字段集不同,合并后快照同时含上游长上下文/模型定价字段与本仓库扩展字段,
// 与任一方旧版本均不兼容,升 22 强制失效。
// v23: issue #171 —— 快照新增 Groups（全部绑定分组）与 User.UserGroupRPMOverrides
// （按分组的 RPM override）。**必须升版**：存量 v22 快照没有 Groups 字段,
// 反序列化后会得到 len(Groups)==0,而这在 v23 的语义里表示「未分组 Key」——
// 于是多分组 Key 会在 L2 TTL 内静默退化成单分组（按默认组计费、选组失效),
// 无报错无日志。这是本次上线期最隐蔽的坑。
// 注:本仓库与上游各自独立演进该版本号,每次 sync 合并若双方都动过快照结构,需继续递增。
const apiKeyAuthSnapshotVersion = 23

type apiKeyAuthCacheConfig struct {
	l1Size        int
	l1TTL         time.Duration
	l2TTL         time.Duration
	negativeTTL   time.Duration
	jitterPercent int
	singleflight  bool
}

func newAPIKeyAuthCacheConfig(cfg *config.Config) apiKeyAuthCacheConfig {
	if cfg == nil {
		return apiKeyAuthCacheConfig{}
	}
	auth := cfg.APIKeyAuth
	return apiKeyAuthCacheConfig{
		l1Size:        auth.L1Size,
		l1TTL:         time.Duration(auth.L1TTLSeconds) * time.Second,
		l2TTL:         time.Duration(auth.L2TTLSeconds) * time.Second,
		negativeTTL:   time.Duration(auth.NegativeTTLSeconds) * time.Second,
		jitterPercent: auth.JitterPercent,
		singleflight:  auth.Singleflight,
	}
}

func (c apiKeyAuthCacheConfig) l1Enabled() bool {
	return c.l1Size > 0 && c.l1TTL > 0
}

func (c apiKeyAuthCacheConfig) l2Enabled() bool {
	return c.l2TTL > 0
}

func (c apiKeyAuthCacheConfig) negativeEnabled() bool {
	return c.negativeTTL > 0
}

// jitterTTL 为缓存 TTL 添加抖动，避免多个请求在同一时刻同时过期触发集中回源。
// 这里直接使用 rand/v2 的顶层函数：并发安全，无需全局互斥锁。
func (c apiKeyAuthCacheConfig) jitterTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	if c.jitterPercent <= 0 {
		return ttl
	}
	percent := c.jitterPercent
	if percent > 100 {
		percent = 100
	}
	delta := float64(percent) / 100
	randVal := rand.Float64()
	factor := 1 - delta + randVal*(2*delta)
	if factor <= 0 {
		return ttl
	}
	return time.Duration(float64(ttl) * factor)
}

func (s *APIKeyService) initAuthCache(cfg *config.Config) {
	s.authCfg = newAPIKeyAuthCacheConfig(cfg)
	if s.authCfg.negativeEnabled() {
		negativeSize := defaultNegativeAuthCacheSize
		if s.authCfg.l1Size > 0 && s.authCfg.l1Size < negativeSize {
			negativeSize = s.authCfg.l1Size
		}
		cache, err := ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(negativeSize) * 10,
			MaxCost:     int64(negativeSize),
			BufferItems: 64,
		})
		if err == nil {
			s.authNegativeCacheL1 = cache
		}
	}
	if s.authCfg.l1Enabled() {
		cache, err := ristretto.NewCache(&ristretto.Config{
			NumCounters: int64(s.authCfg.l1Size) * 10,
			MaxCost:     int64(s.authCfg.l1Size),
			BufferItems: 64,
		})
		if err == nil {
			s.authCacheL1 = cache
		}
	}
}

// StartAuthCacheInvalidationSubscriber starts the Pub/Sub subscriber for L1 cache invalidation.
// This should be called after the service is fully initialized.
func (s *APIKeyService) StartAuthCacheInvalidationSubscriber(ctx context.Context) {
	if s.cache == nil || (s.authCacheL1 == nil && s.authNegativeCacheL1 == nil) {
		return
	}
	s.authInvalidationStart.Do(func() {
		subscriberCtx, cancel := context.WithCancel(ctx)
		subscriberCtx = withAuthCacheSubscriptionReady(subscriberCtx, func() {
			s.authInvalidationConnected.Store(true)
		})
		s.authInvalidationCancel = cancel
		s.authInvalidationWG.Add(1)
		go func() {
			defer s.authInvalidationWG.Done()
			backoff := time.Second
			for {
				err := s.cache.SubscribeAuthCacheInvalidation(subscriberCtx, func(cacheKey string) {
					s.invalidateLocalAuthCache(cacheKey)
				})
				wasConnected := s.authInvalidationConnected.Swap(false)
				if subscriberCtx.Err() != nil {
					return
				}
				if wasConnected {
					backoff = time.Second
				}
				s.authInvalidationFailures.Add(1)
				if err == nil {
					err = errors.New("auth cache invalidation subscription closed")
				}
				slog.Warn("failed to start auth cache invalidation subscriber; retrying", "error", err, "retry_in", backoff)
				timer := time.NewTimer(backoff)
				select {
				case <-subscriberCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if backoff < 30*time.Second {
					backoff *= 2
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
				}
			}
		}()
	})
}

func (s *APIKeyService) invalidateLocalAuthCache(cacheKey string) {
	if s == nil {
		return
	}
	if s.authCacheL1 != nil {
		s.authCacheL1.Del(cacheKey)
	}
	if s.authNegativeCacheL1 != nil {
		s.authNegativeCacheL1.Del(cacheKey)
	}
}

type AuthCacheInvalidationSubscriberHealth struct {
	Connected bool   `json:"connected"`
	Failures  uint64 `json:"failures"`
}

func (s *APIKeyService) AuthCacheInvalidationSubscriberHealth() AuthCacheInvalidationSubscriberHealth {
	if s == nil {
		return AuthCacheInvalidationSubscriberHealth{}
	}
	return AuthCacheInvalidationSubscriberHealth{
		Connected: s.authInvalidationConnected.Load(),
		Failures:  s.authInvalidationFailures.Load(),
	}
}

func (s *APIKeyService) StopAuthCacheInvalidationSubscriber() {
	if s == nil {
		return
	}
	s.authInvalidationStop.Do(func() {
		if s.authInvalidationCancel != nil {
			s.authInvalidationCancel()
		}
		s.authInvalidationWG.Wait()
	})
}

func (s *APIKeyService) authCacheKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *APIKeyService) getAuthCacheEntry(ctx context.Context, cacheKey string) (*APIKeyAuthCacheEntry, bool) {
	if s.authCacheL1 != nil {
		if val, ok := s.authCacheL1.Get(cacheKey); ok {
			if entry, ok := val.(*APIKeyAuthCacheEntry); ok {
				return entry, true
			}
		}
	}
	if s.authNegativeCacheL1 != nil {
		if val, ok := s.authNegativeCacheL1.Get(cacheKey); ok {
			if entry, ok := val.(*APIKeyAuthCacheEntry); ok && entry.NotFound {
				return entry, true
			}
		}
	}
	if s.cache == nil || !s.authCfg.l2Enabled() {
		return nil, false
	}
	entry, err := s.cache.GetAuthCache(ctx, cacheKey)
	if err != nil {
		return nil, false
	}
	s.setAuthCacheL1(cacheKey, entry)
	return entry, true
}

func (s *APIKeyService) setAuthCacheL1(cacheKey string, entry *APIKeyAuthCacheEntry) {
	if entry == nil {
		return
	}
	if entry.NotFound {
		if s.authNegativeCacheL1 != nil && s.authCfg.negativeTTL > 0 {
			_ = s.authNegativeCacheL1.SetWithTTL(cacheKey, entry, 1, s.authCfg.jitterTTL(s.authCfg.negativeTTL))
		}
		return
	}
	if s.authCacheL1 == nil {
		return
	}
	ttl := s.authCfg.l1TTL
	ttl = s.authCfg.jitterTTL(ttl)
	_ = s.authCacheL1.SetWithTTL(cacheKey, entry, 1, ttl)
}

func (s *APIKeyService) setAuthCacheEntry(ctx context.Context, cacheKey string, entry *APIKeyAuthCacheEntry, ttl time.Duration) {
	if entry == nil {
		return
	}
	s.setAuthCacheL1(cacheKey, entry)
	if s.cache == nil || !s.authCfg.l2Enabled() {
		return
	}
	_ = s.cache.SetAuthCache(ctx, cacheKey, entry, s.authCfg.jitterTTL(ttl))
}

func (s *APIKeyService) deleteAuthCache(ctx context.Context, cacheKey string) {
	if s.authCacheL1 != nil {
		s.authCacheL1.Del(cacheKey)
	}
	if s.authNegativeCacheL1 != nil {
		s.authNegativeCacheL1.Del(cacheKey)
	}
	if s.cache == nil {
		return
	}
	_ = s.cache.DeleteAuthCache(ctx, cacheKey)
	// Publish invalidation message to other instances
	_ = s.cache.PublishAuthCacheInvalidation(ctx, cacheKey)
}

func (s *APIKeyService) loadAuthCacheEntry(ctx context.Context, key, cacheKey string) (*APIKeyAuthCacheEntry, error) {
	apiKey, err := s.lookupAPIKeyForAuth(ctx, key)
	if err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			entry := &APIKeyAuthCacheEntry{NotFound: true}
			if s.authCfg.negativeEnabled() {
				// Invalid keys are attacker-controlled and high-cardinality. Keep their
				// negative entries in the bounded process-local cache; do not amplify
				// random-key scans into Redis writes on every instance.
				s.setAuthCacheL1(cacheKey, entry)
			}
			return entry, nil
		}
		return nil, fmt.Errorf("get api key: %w", err)
	}
	apiKey.Key = key
	snapshot := s.snapshotFromAPIKey(ctx, apiKey)
	if snapshot == nil {
		return nil, fmt.Errorf("get api key: %w", ErrAPIKeyNotFound)
	}
	entry := &APIKeyAuthCacheEntry{Snapshot: snapshot}
	s.setAuthCacheEntry(ctx, cacheKey, entry, s.authCfg.l2TTL)
	return entry, nil
}

func (s *APIKeyService) lookupAPIKeyForAuth(ctx context.Context, key string) (*APIKey, error) {
	if s == nil || s.apiKeyRepo == nil {
		return nil, ErrAPIKeyNotFound
	}
	if s.authLookupSlots == nil {
		return s.apiKeyRepo.GetByKeyForAuth(ctx, key)
	}
	s.authLookupTotal.Add(1)
	select {
	case s.authLookupSlots <- struct{}{}:
		s.authLookupInFlight.Add(1)
		defer func() {
			s.authLookupInFlight.Add(-1)
			<-s.authLookupSlots
		}()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		s.authLookupRejected.Add(1)
		return nil, ErrAPIKeyAuthOverloaded
	}
	return s.apiKeyRepo.GetByKeyForAuth(ctx, key)
}

func (s *APIKeyService) applyAuthCacheEntry(key string, entry *APIKeyAuthCacheEntry) (*APIKey, bool, error) {
	if entry == nil {
		return nil, false, nil
	}
	if entry.NotFound {
		return nil, true, ErrAPIKeyNotFound
	}
	if entry.Snapshot == nil {
		return nil, false, nil
	}
	if entry.Snapshot.Version != apiKeyAuthSnapshotVersion {
		return nil, false, nil
	}
	return s.snapshotToAPIKey(key, entry.Snapshot), true, nil
}

func (s *APIKeyService) snapshotFromAPIKey(ctx context.Context, apiKey *APIKey) *APIKeyAuthSnapshot {
	if apiKey == nil || apiKey.User == nil {
		return nil
	}
	snapshot := &APIKeyAuthSnapshot{
		Version:       apiKeyAuthSnapshotVersion,
		APIKeyID:      apiKey.ID,
		UserID:        apiKey.UserID,
		GroupID:       apiKey.GroupID,
		Name:          apiKey.Name,
		Status:        apiKey.Status,
		IPWhitelist:   apiKey.IPWhitelist,
		IPBlacklist:   apiKey.IPBlacklist,
		Quota:         apiKey.Quota,
		QuotaUsed:     apiKey.QuotaUsed,
		ExpiresAt:     apiKey.ExpiresAt,
		RateLimit5h:   apiKey.RateLimit5h,
		RateLimit1d:   apiKey.RateLimit1d,
		RateLimit7d:   apiKey.RateLimit7d,
		Usage1d:       apiKey.Usage1d,
		Window1dStart: apiKey.Window1dStart,
		Usage7d:       apiKey.Usage7d,
		Window7dStart: apiKey.Window7dStart,
		User: APIKeyAuthUserSnapshot{
			ID:                         apiKey.User.ID,
			Status:                     apiKey.User.Status,
			Role:                       apiKey.User.Role,
			Balance:                    apiKey.User.Balance,
			Concurrency:                apiKey.User.Concurrency,
			AllowedGroups:              apiKey.User.AllowedGroups,
			Email:                      apiKey.User.Email,
			Username:                   apiKey.User.Username,
			SignupSource:               apiKey.User.SignupSource,
			BalanceNotifyEnabled:       apiKey.User.BalanceNotifyEnabled,
			BalanceNotifyThresholdType: apiKey.User.BalanceNotifyThresholdType,
			BalanceNotifyThreshold:     apiKey.User.BalanceNotifyThreshold,
			BalanceNotifyExtraEmails:   apiKey.User.BalanceNotifyExtraEmails,
			TotalRecharged:             apiKey.User.TotalRecharged,
			RPMLimit:                   apiKey.User.RPMLimit,
		},
	}

	// 填充 (user, group) RPM override —— snapshot 构建时查 DB，后续请求零 DB 往返。
	//
	// 多分组（issue #171）：对**每个绑定分组**各查一次，结果放进 UserGroupRPMOverrides；
	// 默认组的值同时写进旧的单值字段 UserGroupRPMOverride，保持下游兼容。
	// 查询失败或无 override 时**不写入该键**，checkRPM 会回退到 DB 现查
	// （这也是「缺键」必须与「无 override」同义的原因，见字段注释）。
	if s.userGroupRateRepo != nil {
		for _, gid := range authRPMOverrideGroupIDs(apiKey) {
			override, err := s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, apiKey.UserID, gid)
			if err != nil || override == nil {
				continue
			}
			if snapshot.User.UserGroupRPMOverrides == nil {
				snapshot.User.UserGroupRPMOverrides = make(map[int64]int, 1)
			}
			snapshot.User.UserGroupRPMOverrides[gid] = *override
			if apiKey.GroupID != nil && *apiKey.GroupID == gid {
				snapshot.User.UserGroupRPMOverride = override
			}
		}
	}
	if apiKey.Group != nil {
		snapshot.Group = groupToAuthSnapshot(apiKey.Group)
	}
	// 绑定集合（issue #171）。BoundGroups 已由读模型按 (Platform, ID) 稳定排序，
	// 这里保持同序写入 —— 选组依赖这个顺序的稳定性。
	if len(apiKey.BoundGroups) > 0 {
		snapshot.Groups = make([]APIKeyAuthGroupSnapshot, 0, len(apiKey.BoundGroups))
		for _, g := range apiKey.BoundGroups {
			if gs := groupToAuthSnapshot(g); gs != nil {
				snapshot.Groups = append(snapshot.Groups, *gs)
			}
		}
	}
	return snapshot
}

func (s *APIKeyService) snapshotToAPIKey(key string, snapshot *APIKeyAuthSnapshot) *APIKey {
	if snapshot == nil {
		return nil
	}
	apiKey := &APIKey{
		ID:            snapshot.APIKeyID,
		UserID:        snapshot.UserID,
		GroupID:       snapshot.GroupID,
		Key:           key,
		Name:          snapshot.Name,
		Status:        snapshot.Status,
		IPWhitelist:   snapshot.IPWhitelist,
		IPBlacklist:   snapshot.IPBlacklist,
		Quota:         snapshot.Quota,
		QuotaUsed:     snapshot.QuotaUsed,
		ExpiresAt:     snapshot.ExpiresAt,
		RateLimit5h:   snapshot.RateLimit5h,
		RateLimit1d:   snapshot.RateLimit1d,
		RateLimit7d:   snapshot.RateLimit7d,
		Usage1d:       snapshot.Usage1d,
		Window1dStart: snapshot.Window1dStart,
		Usage7d:       snapshot.Usage7d,
		Window7dStart: snapshot.Window7dStart,
		User: &User{
			ID:                         snapshot.User.ID,
			Status:                     snapshot.User.Status,
			Role:                       snapshot.User.Role,
			Balance:                    snapshot.User.Balance,
			Concurrency:                snapshot.User.Concurrency,
			AllowedGroups:              snapshot.User.AllowedGroups,
			Email:                      snapshot.User.Email,
			Username:                   snapshot.User.Username,
			SignupSource:               snapshot.User.SignupSource,
			BalanceNotifyEnabled:       snapshot.User.BalanceNotifyEnabled,
			BalanceNotifyThresholdType: snapshot.User.BalanceNotifyThresholdType,
			BalanceNotifyThreshold:     snapshot.User.BalanceNotifyThreshold,
			BalanceNotifyExtraEmails:   snapshot.User.BalanceNotifyExtraEmails,
			TotalRecharged:             snapshot.User.TotalRecharged,
			RPMLimit:                   snapshot.User.RPMLimit,
			UserGroupRPMOverride:       snapshot.User.UserGroupRPMOverride,
			UserGroupRPMOverrides:      snapshot.User.UserGroupRPMOverrides,
		},
	}
	if snapshot.Group != nil {
		apiKey.Group = authSnapshotToGroup(snapshot.Group)
	}
	// 绑定集合（issue #171）。顺序与写入时一致，选组依赖它稳定。
	if len(snapshot.Groups) > 0 {
		apiKey.BoundGroups = make([]*Group, 0, len(snapshot.Groups))
		for i := range snapshot.Groups {
			if g := authSnapshotToGroup(&snapshot.Groups[i]); g != nil {
				apiKey.BoundGroups = append(apiKey.BoundGroups, g)
			}
		}
	}
	s.compileAPIKeyIPRules(apiKey)
	return apiKey
}

// groupToAuthSnapshot 把分组领域对象转成认证快照的分组部分。
//
// 默认组（snapshot.Group）与全部绑定组（snapshot.Groups）**共用这一个函数**。
// 理由同 authGroupProjection：这些字段每一个都直接喂给热路径上的某道门，
// 漏赋值不报错、只会拿到零值静默失效。两份实现必然漂移，于是会出现
// 「同一分组当默认组时门生效、当非默认绑定组时门失效」这类极难定位的 bug。
//
// 新增字段时：改 APIKeyAuthGroupSnapshot -> 改 authGroupProjection（SQL 投影）
// -> 改本函数与 authSnapshotToGroup（双向转换）-> 补对账测试。
func groupToAuthSnapshot(g *Group) *APIKeyAuthGroupSnapshot {
	if g == nil {
		return nil
	}
	return &APIKeyAuthGroupSnapshot{
		ID:                              g.ID,
		Name:                            g.Name,
		Platform:                        g.Platform,
		IsExclusive:                     g.IsExclusive,
		Status:                          g.Status,
		SubscriptionType:                g.SubscriptionType,
		RateMultiplier:                  g.RateMultiplier,
		DailyLimitUSD:                   g.DailyLimitUSD,
		WeeklyLimitUSD:                  g.WeeklyLimitUSD,
		MonthlyLimitUSD:                 g.MonthlyLimitUSD,
		AllowImageGeneration:            g.AllowImageGeneration,
		AllowBatchImageGeneration:       g.AllowBatchImageGeneration,
		ImageRateIndependent:            g.ImageRateIndependent,
		ImageRateMultiplier:             g.ImageRateMultiplier,
		ImagePrice1K:                    g.ImagePrice1K,
		ImagePrice2K:                    g.ImagePrice2K,
		ImagePrice4K:                    g.ImagePrice4K,
		VideoRateIndependent:            g.VideoRateIndependent,
		VideoRateMultiplier:             g.VideoRateMultiplier,
		VideoPrice480P:                  g.VideoPrice480P,
		VideoPrice720P:                  g.VideoPrice720P,
		VideoPrice1080P:                 g.VideoPrice1080P,
		VideoModelPrices:                NormalizeVideoModelPrices(g.VideoModelPrices),
		WebSearchPricePerCall:           g.WebSearchPricePerCall,
		SearchPricePer1k:                g.SearchPricePer1k,
		AudioRealtimePricePerMin:        g.AudioRealtimePricePerMin,
		AudioTTSPricePerMillionChars:    g.AudioTTSPricePerMillionChars,
		AudioSTTPricePerHour:            g.AudioSTTPricePerHour,
		LongContextPricingEnabled:       g.LongContextPricingEnabled,
		ModelPricing:                    g.ModelPricing,
		ClaudeCodeOnly:                  g.ClaudeCodeOnly,
		CodexCLIOnly:                    g.CodexCLIOnly,
		FallbackGroupID:                 g.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: g.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    g.ModelRouting,
		ModelRoutingEnabled:             g.ModelRoutingEnabled,
		MCPXMLInject:                    g.MCPXMLInject,
		SupportedModelScopes:            g.SupportedModelScopes,
		AllowMessagesDispatch:           g.AllowMessagesDispatch,
		AllowLive:                       g.AllowLive,
		DefaultMappedModel:              g.DefaultMappedModel,
		MessagesDispatchModelConfig:     g.MessagesDispatchModelConfig,
		ModelsListConfig:                g.ModelsListConfig,
		RPMLimit:                        g.RPMLimit,
		MaxReasoningEffort:              g.MaxReasoningEffort,
		ReasoningEffortMappings:         g.ReasoningEffortMappings,
		PeakRateEnabled:                 g.PeakRateEnabled,
		PeakStart:                       g.PeakStart,
		PeakEnd:                         g.PeakEnd,
		PeakRateMultiplier:              g.PeakRateMultiplier,
		ProfitControlEnabled:            g.ProfitControlEnabled,
		ProfitMinMargin:                 g.ProfitMinMargin,
		ProfitSafetyBuffer:              g.ProfitSafetyBuffer,
	}
}

// authSnapshotToGroup 是 groupToAuthSnapshot 的逆向转换，
// 默认组与全部绑定组**共用这一个函数**（理由见 groupToAuthSnapshot）。
//
// 注意 Hydrated: true —— 这是从快照还原的分组唯一能标记「字段已完整装载」的地方。
// 漏掉它会让 service.IsGroupContextValid 判假，于是 setGroupContext 静默跳过，
// 计费 fallback 到调度分组：**无报错、无日志的静默错价**。
func authSnapshotToGroup(s *APIKeyAuthGroupSnapshot) *Group {
	if s == nil {
		return nil
	}
	return &Group{
		ID:                              s.ID,
		Name:                            s.Name,
		Platform:                        s.Platform,
		IsExclusive:                     s.IsExclusive,
		Status:                          s.Status,
		Hydrated:                        true,
		SubscriptionType:                s.SubscriptionType,
		RateMultiplier:                  s.RateMultiplier,
		DailyLimitUSD:                   s.DailyLimitUSD,
		WeeklyLimitUSD:                  s.WeeklyLimitUSD,
		MonthlyLimitUSD:                 s.MonthlyLimitUSD,
		AllowImageGeneration:            s.AllowImageGeneration,
		AllowBatchImageGeneration:       s.AllowBatchImageGeneration,
		ImageRateIndependent:            s.ImageRateIndependent,
		ImageRateMultiplier:             s.ImageRateMultiplier,
		ImagePrice1K:                    s.ImagePrice1K,
		ImagePrice2K:                    s.ImagePrice2K,
		ImagePrice4K:                    s.ImagePrice4K,
		VideoRateIndependent:            s.VideoRateIndependent,
		VideoRateMultiplier:             s.VideoRateMultiplier,
		VideoPrice480P:                  s.VideoPrice480P,
		VideoPrice720P:                  s.VideoPrice720P,
		VideoPrice1080P:                 s.VideoPrice1080P,
		VideoModelPrices:                NormalizeVideoModelPrices(s.VideoModelPrices),
		WebSearchPricePerCall:           s.WebSearchPricePerCall,
		SearchPricePer1k:                s.SearchPricePer1k,
		AudioRealtimePricePerMin:        s.AudioRealtimePricePerMin,
		AudioTTSPricePerMillionChars:    s.AudioTTSPricePerMillionChars,
		AudioSTTPricePerHour:            s.AudioSTTPricePerHour,
		LongContextPricingEnabled:       s.LongContextPricingEnabled,
		ModelPricing:                    s.ModelPricing,
		ClaudeCodeOnly:                  s.ClaudeCodeOnly,
		CodexCLIOnly:                    s.CodexCLIOnly,
		FallbackGroupID:                 s.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: s.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    s.ModelRouting,
		ModelRoutingEnabled:             s.ModelRoutingEnabled,
		MCPXMLInject:                    s.MCPXMLInject,
		SupportedModelScopes:            s.SupportedModelScopes,
		AllowMessagesDispatch:           s.AllowMessagesDispatch,
		AllowLive:                       s.AllowLive,
		DefaultMappedModel:              s.DefaultMappedModel,
		MessagesDispatchModelConfig:     s.MessagesDispatchModelConfig,
		ModelsListConfig:                s.ModelsListConfig,
		RPMLimit:                        s.RPMLimit,
		MaxReasoningEffort:              s.MaxReasoningEffort,
		ReasoningEffortMappings:         s.ReasoningEffortMappings,
		PeakRateEnabled:                 s.PeakRateEnabled,
		PeakStart:                       s.PeakStart,
		PeakEnd:                         s.PeakEnd,
		PeakRateMultiplier:              s.PeakRateMultiplier,
		ProfitControlEnabled:            s.ProfitControlEnabled,
		ProfitMinMargin:                 s.ProfitMinMargin,
		ProfitSafetyBuffer:              s.ProfitSafetyBuffer,
	}
}

// authRPMOverrideGroupIDs 返回构建快照时需要查 (user, group) RPM override 的分组 ID 集合：
// 默认组 + 全部绑定组，去重后按升序（顺序固定是为了让快照与测试可复现）。
//
// 单分组 Key 只会得到一个 ID，查询次数与改造前逐字相同（C3）。
// 多分组 Key 的查询次数等于绑定组数，上界是平台总数（见 apiKeyMaxBoundGroups），
// 且只发生在快照回源时（L1/L2 未命中），不在每请求路径上。
func authRPMOverrideGroupIDs(apiKey *APIKey) []int64 {
	if apiKey == nil {
		return nil
	}
	seen := make(map[int64]struct{}, len(apiKey.BoundGroups)+1)
	ids := make([]int64, 0, len(apiKey.BoundGroups)+1)
	add := func(id int64) {
		if id <= 0 {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if apiKey.GroupID != nil {
		add(*apiKey.GroupID)
	}
	for _, g := range apiKey.BoundGroups {
		if g != nil {
			add(g.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
