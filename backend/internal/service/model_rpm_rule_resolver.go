package service

import (
	"context"
	"sort"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	gocache "github.com/patrickmn/go-cache"
	"golang.org/x/sync/singleflight"
)

const (
	// 规则总量为几十条量级，全表加载进内存；30s TTL 与 user_group_rate_resolver 同一套路。
	defaultModelRPMRuleCacheTTL = 30 * time.Second
	modelRPMRuleCacheKey        = "model_rpm_rules:all"
	modelRPMRuleLoadTimeout     = 3 * time.Second
)

var (
	modelRPMRuleCacheHitTotal      atomic.Int64
	modelRPMRuleCacheMissTotal     atomic.Int64
	modelRPMRuleCacheLoadTotal     atomic.Int64
	modelRPMRuleCacheSFSharedTotal atomic.Int64
	modelRPMRuleCacheFallbackTotal atomic.Int64
)

// ModelRPMRuleCacheStats 暴露规则快照缓存的命中情况，供 ops 面板排查「改了规则没生效」。
func ModelRPMRuleCacheStats() (cacheHit, cacheMiss, load, singleflightShared, fallback int64) {
	return modelRPMRuleCacheHitTotal.Load(),
		modelRPMRuleCacheMissTotal.Load(),
		modelRPMRuleCacheLoadTotal.Load(),
		modelRPMRuleCacheSFSharedTotal.Load(),
		modelRPMRuleCacheFallbackTotal.Load()
}

// ModelRPMRuleResolver 维护模型 RPM 规则的内存快照。
//
// 一致性：管理端写入后主动失效本副本缓存；多副本靠 TTL 最终一致，
// 因此改规则在其它副本上最长 30s 生效。规则是限流配置而非授权边界，
// 该延迟可接受（与 user_group_rate_resolver 的取舍一致）。
type ModelRPMRuleResolver struct {
	repo     ModelRPMRuleRepository
	cache    *gocache.Cache
	cacheTTL time.Duration
	sf       *singleflight.Group
	// lastGood 保留最近一次成功加载的快照：DB 抖动时继续用旧快照，
	// 而不是退化成「零规则 = 不限流」。
	lastGood atomic.Pointer[[]ModelRPMRule]
}

// NewModelRPMRuleResolver 创建规则快照解析器。
func NewModelRPMRuleResolver(repo ModelRPMRuleRepository) *ModelRPMRuleResolver {
	return &ModelRPMRuleResolver{
		repo:     repo,
		cache:    gocache.New(defaultModelRPMRuleCacheTTL, time.Minute),
		cacheTTL: defaultModelRPMRuleCacheTTL,
		sf:       &singleflight.Group{},
	}
}

// Invalidate 失效本副本的规则快照（管理端写入后调用）。
func (r *ModelRPMRuleResolver) Invalidate() {
	if r == nil || r.cache == nil {
		return
	}
	r.cache.Delete(modelRPMRuleCacheKey)
}

// Snapshot 返回当前启用的规则快照，已按「target 具体度降序、id 升序」排好序。
// 返回的切片只读，调用方不得修改。
func (r *ModelRPMRuleResolver) Snapshot(ctx context.Context) []ModelRPMRule {
	if r == nil || r.repo == nil {
		return nil
	}

	if r.cache != nil {
		if cached, ok := r.cache.Get(modelRPMRuleCacheKey); ok {
			if rules, castOK := cached.([]ModelRPMRule); castOK {
				modelRPMRuleCacheHitTotal.Add(1)
				return rules
			}
		}
	}
	modelRPMRuleCacheMissTotal.Add(1)

	value, err, shared := r.sf.Do(modelRPMRuleCacheKey, func() (any, error) {
		if r.cache != nil {
			if cached, ok := r.cache.Get(modelRPMRuleCacheKey); ok {
				if rules, castOK := cached.([]ModelRPMRule); castOK {
					modelRPMRuleCacheHitTotal.Add(1)
					return rules, nil
				}
			}
		}

		modelRPMRuleCacheLoadTotal.Add(1)
		// 与请求 ctx 解耦：一个被取消的请求不该让其它等待同一次加载的请求一起失败。
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelRPMRuleLoadTimeout)
		defer cancel()

		all, repoErr := r.repo.ListAll(loadCtx)
		if repoErr != nil {
			return nil, repoErr
		}

		enabled := make([]ModelRPMRule, 0, len(all))
		for _, rule := range all {
			if rule.Enabled {
				enabled = append(enabled, rule)
			}
		}
		sortModelRPMRules(enabled)

		if r.cache != nil {
			r.cache.Set(modelRPMRuleCacheKey, enabled, r.cacheTTL)
		}
		r.lastGood.Store(&enabled)
		return enabled, nil
	})
	if shared {
		modelRPMRuleCacheSFSharedTotal.Add(1)
	}
	if err != nil {
		modelRPMRuleCacheFallbackTotal.Add(1)
		logger.LegacyPrintf("service.billing_cache", "Warning: load model rpm rules failed, reusing last snapshot: %v", err)
		if last := r.lastGood.Load(); last != nil {
			return *last
		}
		return nil
	}

	rules, ok := value.([]ModelRPMRule)
	if !ok {
		modelRPMRuleCacheFallbackTotal.Add(1)
		return nil
	}
	return rules
}

// sortModelRPMRules 确定性排序：target 具体度降序（user > group > all），同具体度按 id 升序。
// 「首次超限即返回」依赖这个顺序，否则多规则命中时被拒的原因随机漂移。
func sortModelRPMRules(rules []ModelRPMRule) {
	sort.SliceStable(rules, func(i, j int) bool {
		si, sj := rules[i].targetSpecificity(), rules[j].targetSpecificity()
		if si != sj {
			return si > sj
		}
		return rules[i].ID < rules[j].ID
	})
}
