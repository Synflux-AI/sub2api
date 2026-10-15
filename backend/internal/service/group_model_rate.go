package service

import (
	"math"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// GroupModelRateMultiplier 分组逐模型售价倍率规则。
type GroupModelRateMultiplier = domain.GroupModelRateMultiplier

// BillingRateSource 标识一次计费所用 base 倍率（不含高峰因子）的来源层。
type BillingRateSource string

// base 倍率解析顺序（全部为替换，不相乘），取第一个命中的：
//
//  1. user_model  用户在该分组改过的模型倍率
//  2. group_model 分组模型倍率（用户未改＝继承）
//  3. user        用户专属倍率
//  4. group       分组默认倍率
//  5. system      系统默认（无分组）
const (
	BillingRateSourceUserModel  BillingRateSource = "user_model"
	BillingRateSourceGroupModel BillingRateSource = "group_model"
	BillingRateSourceUser       BillingRateSource = "user"
	BillingRateSourceGroup      BillingRateSource = "group"
	BillingRateSourceSystem     BillingRateSource = "system"
)

// MatchModelRateMultiplier 返回 model 命中的分组模型倍率规则：pattern 为规则中原样保存的模式串，
// 用户覆盖以它为键。匹配语义与 matchGroupModelPricing 一致：先在全部规则里找精确匹配，
// 再取第一个命中的 `前缀*` 通配。
func (g *Group) MatchModelRateMultiplier(model string) (pattern string, rate float64, ok bool) {
	if g == nil || len(g.ModelRateMultipliers) == 0 {
		return "", 0, false
	}
	model = normalizeChannelPricingModelName(model)
	if model == "" {
		return "", 0, false
	}
	wildcardPattern, wildcardRate, wildcardOK := "", 0.0, false
	for _, rule := range g.ModelRateMultipliers {
		for _, p := range rule.Models {
			normalized := normalizeChannelPricingModelName(p)
			if normalized == model {
				return p, rule.RateMultiplier, true
			}
			if !wildcardOK && strings.HasSuffix(normalized, "*") && strings.HasPrefix(model, strings.TrimSuffix(normalized, "*")) {
				wildcardPattern, wildcardRate, wildcardOK = p, rule.RateMultiplier, true
			}
		}
	}
	return wildcardPattern, wildcardRate, wildcardOK
}

// ModelRatePatterns 返回分组全部模型倍率规则的模式串（原样）。
func (g *Group) ModelRatePatterns() []string {
	if g == nil {
		return nil
	}
	var out []string
	for _, rule := range g.ModelRateMultipliers {
		out = append(out, rule.Models...)
	}
	return out
}

// resolveBillingBaseRate 按上面的五层顺序解析 base 倍率。group 为 nil 时由调用方使用系统默认。
// userModelRates 以分组规则的模式串为键；只有 model 命中的那条分组规则的模式串才会被查，
// 因此分组里已不存在的模式串上的用户覆盖天然不生效。
func resolveBillingBaseRate(group *Group, userRate *float64, userModelRates map[string]float64, model string) (float64, BillingRateSource) {
	if pattern, groupModelRate, ok := group.MatchModelRateMultiplier(model); ok {
		if v, has := userModelRates[pattern]; has {
			return v, BillingRateSourceUserModel
		}
		return groupModelRate, BillingRateSourceGroupModel
	}
	if userRate != nil {
		return *userRate, BillingRateSourceUser
	}
	return group.RateMultiplier, BillingRateSourceGroup
}

func validBillingRateMultiplier(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0
}

func validateModelRatePattern(pattern string) error {
	if pattern == "" {
		return infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_RATE_MODEL_REQUIRED", "model rate multiplier pattern must not be empty")
	}
	if pattern == "*" {
		return infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_RATE_PATTERN_INVALID", "model rate multiplier pattern must not be a bare '*'; use the group rate_multiplier instead")
	}
	if idx := strings.Index(pattern, "*"); idx >= 0 && idx != len(pattern)-1 {
		return infraerrors.Newf(http.StatusBadRequest, "GROUP_MODEL_RATE_PATTERN_INVALID", "model rate multiplier pattern %q may only use '*' as a trailing wildcard", pattern)
	}
	return nil
}

// normalizeGroupModelRateMultipliers 是分组模型倍率规则落库前的唯一收口：去除模式串首尾空白，
// 校验模式串与倍率，并拒绝在多条规则（或同一条规则内）重复出现的模式串。
func normalizeGroupModelRateMultipliers(rules []GroupModelRateMultiplier) ([]GroupModelRateMultiplier, error) {
	out := make([]GroupModelRateMultiplier, 0, len(rules))
	seen := make(map[string]struct{})
	for _, rule := range rules {
		if !validBillingRateMultiplier(rule.RateMultiplier) {
			return nil, infraerrors.Newf(http.StatusBadRequest, "GROUP_MODEL_RATE_INVALID", "model rate multiplier must be a finite number > 0, got %v", rule.RateMultiplier)
		}
		models := make([]string, 0, len(rule.Models))
		for _, raw := range rule.Models {
			pattern := strings.TrimSpace(raw)
			if err := validateModelRatePattern(pattern); err != nil {
				return nil, err
			}
			key := normalizeChannelPricingModelName(pattern)
			if _, dup := seen[key]; dup {
				return nil, infraerrors.Newf(http.StatusBadRequest, "GROUP_MODEL_RATE_PATTERN_DUPLICATE", "model rate multiplier pattern %q appears more than once", pattern)
			}
			seen[key] = struct{}{}
			models = append(models, pattern)
		}
		if len(models) == 0 {
			return nil, infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_RATE_MODEL_REQUIRED", "model rate multiplier entry requires at least one model")
		}
		out = append(out, GroupModelRateMultiplier{Models: models, RateMultiplier: rule.RateMultiplier})
	}
	return out, nil
}

// normalizeUserGroupModelRates 校验用户对分组模型倍率的覆盖：键必须是分组现有规则的模式串
// （按定价模型名归一化比较，落库时改写为分组里原样保存的模式串），倍率必须合法。
// 用户层只能修改分组已有的规则，不能新增分组没有的模型。
func normalizeUserGroupModelRates(group *Group, overrides map[string]float64) (map[string]float64, error) {
	byKey := make(map[string]string)
	for _, p := range group.ModelRatePatterns() {
		byKey[normalizeChannelPricingModelName(p)] = p
	}
	out := make(map[string]float64, len(overrides))
	for raw, rate := range overrides {
		pattern, ok := byKey[normalizeChannelPricingModelName(raw)]
		if !ok {
			return nil, infraerrors.Newf(http.StatusBadRequest, "USER_MODEL_RATE_PATTERN_UNKNOWN", "model %q is not configured in the group's model rate multipliers", strings.TrimSpace(raw))
		}
		if !validBillingRateMultiplier(rate) {
			return nil, infraerrors.Newf(http.StatusBadRequest, "USER_MODEL_RATE_INVALID", "model rate multiplier for %q must be a finite number > 0, got %v", pattern, rate)
		}
		if _, dup := out[pattern]; dup {
			return nil, infraerrors.Newf(http.StatusBadRequest, "USER_MODEL_RATE_PATTERN_DUPLICATE", "model %q is specified more than once", pattern)
		}
		out[pattern] = rate
	}
	return out, nil
}

func cloneGroupModelRateMultipliers(rules []GroupModelRateMultiplier) []GroupModelRateMultiplier {
	if rules == nil {
		return nil
	}
	out := make([]GroupModelRateMultiplier, len(rules))
	for i, rule := range rules {
		out[i] = GroupModelRateMultiplier{Models: append([]string(nil), rule.Models...), RateMultiplier: rule.RateMultiplier}
	}
	return out
}

// NormalizeGroupModelRateMultipliersForStorage 返回可直接落库的规则切片：nil 写成空数组，
// 与列默认值 '[]' 保持一致。
func NormalizeGroupModelRateMultipliersForStorage(rules []GroupModelRateMultiplier) []GroupModelRateMultiplier {
	if rules == nil {
		return []GroupModelRateMultiplier{}
	}
	return rules
}
