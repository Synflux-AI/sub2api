package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	gocache "github.com/patrickmn/go-cache"
	"golang.org/x/sync/singleflight"
)

type userGroupRateResolver struct {
	repo         UserGroupRateRepository
	cache        *gocache.Cache
	cacheTTL     time.Duration
	sf           *singleflight.Group
	logComponent string
}

func newUserGroupRateResolver(repo UserGroupRateRepository, cache *gocache.Cache, cacheTTL time.Duration, sf *singleflight.Group, logComponent string) *userGroupRateResolver {
	if cacheTTL <= 0 {
		cacheTTL = defaultUserGroupRateCacheTTL
	}
	if cache == nil {
		cache = gocache.New(cacheTTL, time.Minute)
	}
	if logComponent == "" {
		logComponent = "service.gateway"
	}
	if sf == nil {
		sf = &singleflight.Group{}
	}

	return &userGroupRateResolver{
		repo:         repo,
		cache:        cache,
		cacheTTL:     cacheTTL,
		sf:           sf,
		logComponent: logComponent,
	}
}

// Resolve 返回用户在分组的 base 倍率（用户专属 ?? 分组默认），不看模型。
// 计费路径应使用 ResolveForModel；本方法保留给尚未按模型区分的调用方。
func (r *userGroupRateResolver) Resolve(ctx context.Context, userID, groupID int64, groupDefaultMultiplier float64) float64 {
	cfg := r.load(ctx, userID, groupID)
	if cfg != nil && cfg.RateMultiplier != nil {
		return *cfg.RateMultiplier
	}
	return groupDefaultMultiplier
}

// ResolveForModel 按 resolveBillingBaseRate 的五层顺序解析 base 倍率（不含高峰因子）。
// group 为 nil 时返回 systemDefault。缓存只存用户配置，模型匹配在每次调用时现算，
// 因此缓存条目数不随模型维度膨胀，分组规则或默认倍率变更也不受用户配置缓存影响。
func (r *userGroupRateResolver) ResolveForModel(ctx context.Context, userID int64, group *Group, model string, systemDefault float64) (float64, BillingRateSource) {
	if group == nil {
		return systemDefault, BillingRateSourceSystem
	}
	cfg := r.load(ctx, userID, group.ID)
	if cfg == nil {
		return resolveBillingBaseRate(group, nil, nil, model)
	}
	return resolveBillingBaseRate(group, cfg.RateMultiplier, cfg.ModelRateMultipliers, model)
}

// load 读取（并缓存）用户在分组的倍率配置；无配置、无仓储或读取失败时返回 nil（调用方回落分组）。
func (r *userGroupRateResolver) load(ctx context.Context, userID, groupID int64) *UserGroupRateConfig {
	if r == nil || userID <= 0 || groupID <= 0 {
		return nil
	}

	key := fmt.Sprintf("%d:%d", userID, groupID)
	if cfg, ok := r.cached(key); ok {
		userGroupRateCacheHitTotal.Add(1)
		return cfg
	}
	if r.repo == nil {
		return nil
	}
	userGroupRateCacheMissTotal.Add(1)

	value, err, shared := r.sf.Do(key, func() (any, error) {
		if cfg, ok := r.cached(key); ok {
			userGroupRateCacheHitTotal.Add(1)
			return cfg, nil
		}

		userGroupRateCacheLoadTotal.Add(1)
		cfg, repoErr := r.fetch(ctx, userID, groupID)
		if repoErr != nil {
			return nil, repoErr
		}
		if r.cache != nil {
			r.cache.Set(key, cfg, r.cacheTTL)
		}
		return cfg, nil
	})
	if shared {
		userGroupRateCacheSFSharedTotal.Add(1)
	}
	if err != nil {
		userGroupRateCacheFallbackTotal.Add(1)
		logger.LegacyPrintf(r.logComponent, "get user group rate failed, fallback to group default: user=%d group=%d err=%v", userID, groupID, err)
		return nil
	}

	cfg, ok := value.(*UserGroupRateConfig)
	if !ok {
		userGroupRateCacheFallbackTotal.Add(1)
		return nil
	}
	return cfg
}

// fetch 优先一次读出专属倍率与模型覆盖；仓储未实现模型覆盖能力时只读专属倍率。
// 无行时返回空配置（而非 nil），以便缓存"该用户没有专属配置"。
func (r *userGroupRateResolver) fetch(ctx context.Context, userID, groupID int64) (*UserGroupRateConfig, error) {
	if modelRepo, ok := r.repo.(UserGroupModelRateRepository); ok && modelRepo != nil {
		cfg, err := modelRepo.GetRateConfigByUserAndGroup(ctx, userID, groupID)
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			cfg = &UserGroupRateConfig{}
		}
		return cfg, nil
	}
	rate, err := r.repo.GetByUserAndGroup(ctx, userID, groupID)
	if err != nil {
		return nil, err
	}
	return &UserGroupRateConfig{RateMultiplier: rate}, nil
}

// cached 读取缓存条目；兼容直接写入 float64 专属倍率的旧条目形态。
func (r *userGroupRateResolver) cached(key string) (*UserGroupRateConfig, bool) {
	if r.cache == nil {
		return nil, false
	}
	raw, ok := r.cache.Get(key)
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case *UserGroupRateConfig:
		return v, v != nil
	case float64:
		rate := v
		return &UserGroupRateConfig{RateMultiplier: &rate}, true
	default:
		return nil, false
	}
}
