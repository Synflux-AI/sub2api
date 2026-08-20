package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

const (
	ErrorHandlingActionRetry       = "retry"
	ErrorHandlingActionFailover    = "failover"
	ErrorHandlingActionPassthrough = "passthrough"

	ErrorHandlingExhaustedActionDefault     = "default"
	ErrorHandlingExhaustedActionPassthrough = "passthrough"
)

type ErrorHandlingRule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Enabled 用指针是为了区分「存量配置没有这个字段」和「管理员显式禁用」：
	// 存量 JSON 反序列化成 bool 会得到零值 false，等于把线上所有规则静默停用。
	// nil 一律按启用处理，见 IsEnabled()。
	Enabled *bool `json:"enabled"`
	// Priority 数字越小越先匹配。取代了原先「数组下标即匹配顺序」的隐式约定，
	// 让前端能不改动数组结构就调整顺序。0 表示存量配置没有这个字段，
	// normalize 时按数组下标补 1..N，于是升级后的匹配顺序与升级前逐条一致，无需数据迁移。
	Priority        int      `json:"priority"`
	StatusCodes     []int    `json:"status_codes"`
	Keywords        []string `json:"keywords"`
	Action          string   `json:"action"`
	RetryCount      *int     `json:"retry_count"`
	ExhaustedAction string   `json:"exhausted_action"`
}

type ErrorHandlingRuleSettings struct {
	Enabled           bool                `json:"enabled"`
	DefaultRetryCount int                 `json:"default_retry_count"`
	Rules             []ErrorHandlingRule `json:"rules"`
}

const (
	errorHandlingRuleMaxRules              = 50
	errorHandlingRuleMaxKeywordsPerRule    = 20
	errorHandlingRuleMaxStatusCodesPerRule = 20
	errorHandlingRuleMaxKeywordLen         = 500
	errorHandlingRuleMaxRetryCount         = maxRetryAttempts - 1
	errorHandlingRulePriorityMax           = 999

	errorHandlingRuleCacheTTL  = 60 * time.Second
	errorHandlingRuleErrorTTL  = 5 * time.Second
	errorHandlingRuleDBTimeout = 5 * time.Second
)

type cachedErrorHandlingRuleSettings struct {
	settings  ErrorHandlingRuleSettings
	expiresAt int64
}

// ErrErrorHandlingRuleSettingsInvalid 标记「管理员填错了」这一类错误，让 handler 能把
// 它映射成 400，而把仓储写入失败之类的服务端故障留给 500。
var ErrErrorHandlingRuleSettingsInvalid = errors.New("invalid error handling rule settings")

func DefaultErrorHandlingRuleSettings() *ErrorHandlingRuleSettings {
	return &ErrorHandlingRuleSettings{DefaultRetryCount: 1}
}

func (r *ErrorHandlingRule) RetryLimit(defaultRetryCount int) int {
	if r.RetryCount != nil {
		return *r.RetryCount
	}
	return defaultRetryCount
}

// IsEnabled 报告该规则是否参与匹配。nil 表示存量配置里没有 enabled 字段，按启用处理。
func (r *ErrorHandlingRule) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

// HasEnabledErrorHandlingRule 报告是否存在至少一条启用的规则。全部规则被禁用时，
// 热路径没必要为了 decide 一遍而去读错误响应体。
func HasEnabledErrorHandlingRule(rules []ErrorHandlingRule) bool {
	for i := range rules {
		if rules[i].IsEnabled() {
			return true
		}
	}
	return false
}

func matchErrorHandlingRule(rules []ErrorHandlingRule, statusCode int, respBody []byte) *ErrorHandlingRule {
	if len(rules) == 0 {
		return nil
	}

	var bodyLower string
	bodyLowered := false
	for i := range rules {
		rule := &rules[i]
		if !rule.IsEnabled() {
			continue
		}
		if len(rule.StatusCodes) == 0 && len(rule.Keywords) == 0 {
			continue
		}
		// 未知 action 当作「这条规则不存在」。写入路径有 validate 兜着，但直接改库
		// 或降级回滚留下的旧值会走到这里，而 applyErrorHandlingRule 的 switch 对未知
		// action 没有分支，放行等于掉进 retry 且不消耗重试计数。
		if !isKnownErrorHandlingAction(rule.Action) {
			continue
		}
		if len(rule.StatusCodes) > 0 && !containsStatusCode(rule.StatusCodes, statusCode) {
			continue
		}
		if len(rule.Keywords) > 0 {
			if !bodyLowered {
				bodyLower = strings.ToLower(string(respBody))
				bodyLowered = true
			}
			if !matchKeywordsLowered(bodyLower, rule.Keywords) {
				continue
			}
		}
		return rule
	}
	return nil
}

func isKnownErrorHandlingAction(action string) bool {
	switch action {
	case ErrorHandlingActionRetry, ErrorHandlingActionFailover, ErrorHandlingActionPassthrough:
		return true
	default:
		return false
	}
}

func containsStatusCode(codes []int, statusCode int) bool {
	for _, code := range codes {
		if code == statusCode {
			return true
		}
	}
	return false
}

func matchKeywordsLowered(bodyLower string, keywords []string) bool {
	for _, keyword := range keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword != "" && strings.Contains(bodyLower, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

func normalizeErrorHandlingRules(rules []ErrorHandlingRule) {
	for i := range rules {
		rule := &rules[i]
		rule.Action = strings.TrimSpace(rule.Action)
		if rule.Action == "" {
			rule.Action = ErrorHandlingActionRetry
		}
		if rule.Action != ErrorHandlingActionRetry {
			rule.RetryCount = nil
		}
		rule.ExhaustedAction = strings.TrimSpace(rule.ExhaustedAction)
		if rule.ExhaustedAction == "" {
			rule.ExhaustedAction = ErrorHandlingExhaustedActionDefault
		}
		// 0 是「存量配置没有 priority 字段」的零值，负数只能由手改库产生：
		// 都按数组下标补成 1..N，顺序与本字段落地前逐条一致。
		if rule.Priority <= 0 {
			rule.Priority = i + 1
		}
		// 空 ID 会让 errorHandlingRuleTracker 把不同规则当成同一条（都是 ""），
		// 重试计数被共用。补一个确定性的占位 ID：不用随机值，否则每次读配置
		// 都变一次，日志里同一条规则的 rule_id 对不上。
		//
		// 注意占位 ID 必须在下面的排序之前、按原始下标计算：ID 是
		// errorHandlingRuleTracker 记重试计数的键，排序后再补会让管理员每改一次
		// 优先级就换一次 ID，重试预算串号。
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" {
			rule.ID = fmt.Sprintf("rule-%d", i+1)
		}
	}
	// 把「priority 升序」落成数组顺序，matchErrorHandlingRule 就能继续保持
	// 「按数组顺序取首个命中」的单一语义。normalize 是读写两条路径的必经之地，
	// 所以热路径拿到的缓存数组一定已经是优先级序。
	// 用 SliceStable：相同 priority 是允许的，稳定排序让它们维持提交顺序，
	// 后端匹配顺序才能和前端列表展示顺序完全一致。
	sort.SliceStable(rules, func(i, j int) bool {
		return rules[i].Priority < rules[j].Priority
	})
}

func validateErrorHandlingRuleSettings(settings *ErrorHandlingRuleSettings) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrErrorHandlingRuleSettingsInvalid, fmt.Sprintf(format, args...))
	}
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	if settings.DefaultRetryCount < 0 || settings.DefaultRetryCount > errorHandlingRuleMaxRetryCount {
		return invalid("default_retry_count must be between 0 and %d", errorHandlingRuleMaxRetryCount)
	}
	if len(settings.Rules) > errorHandlingRuleMaxRules {
		return invalid("too many rules (max %d)", errorHandlingRuleMaxRules)
	}
	seenIDs := make(map[string]int, len(settings.Rules))
	for i, rule := range settings.Rules {
		if len(rule.StatusCodes) == 0 && len(rule.Keywords) == 0 {
			return invalid("rule %d: must configure at least one of status_codes or keywords", i+1)
		}
		// 同 ID 的两条规则会共用 errorHandlingRuleTracker 的重试计数。
		if first, dup := seenIDs[rule.ID]; dup {
			return invalid("rule %d: duplicate id %q (already used by rule %d)", i+1, rule.ID, first)
		}
		seenIDs[rule.ID] = i + 1
		if !isKnownErrorHandlingAction(rule.Action) {
			return invalid("rule %d: unknown action %q", i+1, rule.Action)
		}
		if !isKnownErrorHandlingExhaustedAction(rule.ExhaustedAction) {
			return invalid("rule %d: unknown exhausted_action %q", i+1, rule.ExhaustedAction)
		}
		if len(rule.StatusCodes) > errorHandlingRuleMaxStatusCodesPerRule {
			return invalid("rule %d: too many status codes (max %d)", i+1, errorHandlingRuleMaxStatusCodesPerRule)
		}
		for _, code := range rule.StatusCodes {
			if code < 100 || code > 599 {
				return invalid("rule %d: invalid status code %d", i+1, code)
			}
		}
		if len(rule.Keywords) > errorHandlingRuleMaxKeywordsPerRule {
			return invalid("rule %d: too many keywords (max %d)", i+1, errorHandlingRuleMaxKeywordsPerRule)
		}
		for _, keyword := range rule.Keywords {
			if len(keyword) > errorHandlingRuleMaxKeywordLen {
				return invalid("rule %d: keyword too long (max %d characters)", i+1, errorHandlingRuleMaxKeywordLen)
			}
		}
		// validate 在 normalize 之后跑，非正数已经被补成 >=1，所以这里只会拦到
		// 管理员显式填的越界值。
		if rule.Priority < 1 || rule.Priority > errorHandlingRulePriorityMax {
			return invalid("rule %d: priority must be between 1 and %d", i+1, errorHandlingRulePriorityMax)
		}
		if rule.RetryCount != nil && (*rule.RetryCount < 0 || *rule.RetryCount > errorHandlingRuleMaxRetryCount) {
			return invalid("rule %d: retry_count must be between 0 and %d", i+1, errorHandlingRuleMaxRetryCount)
		}
	}
	return nil
}

func isKnownErrorHandlingExhaustedAction(action string) bool {
	switch action {
	case "", ErrorHandlingExhaustedActionDefault, ErrorHandlingExhaustedActionPassthrough:
		return true
	default:
		return false
	}
}

func (s *SettingService) GetErrorHandlingRuleSettings(ctx context.Context) (*ErrorHandlingRuleSettings, error) {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyErrorHandlingRules)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return DefaultErrorHandlingRuleSettings(), nil
		}
		return nil, fmt.Errorf("get error handling rule settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return DefaultErrorHandlingRuleSettings(), nil
	}

	settings := DefaultErrorHandlingRuleSettings()
	if err := json.Unmarshal([]byte(value), settings); err != nil {
		slog.Warn("failed to unmarshal error handling rule settings, falling back to defaults", "error", err, "key", SettingKeyErrorHandlingRules)
		return DefaultErrorHandlingRuleSettings(), nil
	}
	normalizeErrorHandlingRules(settings.Rules)
	return settings, nil
}

func (s *SettingService) SetErrorHandlingRuleSettings(ctx context.Context, settings *ErrorHandlingRuleSettings) error {
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	normalizeErrorHandlingRules(settings.Rules)
	if err := validateErrorHandlingRuleSettings(settings); err != nil {
		return err
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal error handling rule settings: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyErrorHandlingRules, string(data)); err != nil {
		return err
	}
	s.storeErrorHandlingRuleCache(*settings, errorHandlingRuleCacheTTL)
	return nil
}

func (s *SettingService) GetErrorHandlingRuleSettingsCached(ctx context.Context) ErrorHandlingRuleSettings {
	if s == nil || s.settingRepo == nil {
		return *DefaultErrorHandlingRuleSettings()
	}
	if cached, ok := s.errorHandlingRuleCache.Load().(*cachedErrorHandlingRuleSettings); ok && cached != nil && time.Now().UnixNano() < cached.expiresAt {
		return cached.settings
	}

	result, _, _ := s.errorHandlingRuleSF.Do("error_handling_rule_settings", func() (any, error) {
		if cached, ok := s.errorHandlingRuleCache.Load().(*cachedErrorHandlingRuleSettings); ok && cached != nil && time.Now().UnixNano() < cached.expiresAt {
			return cached.settings, nil
		}
		if ctx == nil {
			ctx = context.Background()
		}
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), errorHandlingRuleDBTimeout)
		defer cancel()
		settings, err := s.GetErrorHandlingRuleSettings(dbCtx)
		if err != nil {
			slog.Warn("failed to get error handling rule settings", "error", err)
			fallback := *DefaultErrorHandlingRuleSettings()
			if prior, ok := s.errorHandlingRuleCache.Load().(*cachedErrorHandlingRuleSettings); ok && prior != nil {
				fallback = prior.settings
			}
			s.storeErrorHandlingRuleCache(fallback, errorHandlingRuleErrorTTL)
			return fallback, nil
		}
		s.storeErrorHandlingRuleCache(*settings, errorHandlingRuleCacheTTL)
		return *settings, nil
	})
	if settings, ok := result.(ErrorHandlingRuleSettings); ok {
		return settings
	}
	return *DefaultErrorHandlingRuleSettings()
}

func (s *SettingService) storeErrorHandlingRuleCache(settings ErrorHandlingRuleSettings, ttl time.Duration) {
	s.errorHandlingRuleCache.Store(&cachedErrorHandlingRuleSettings{settings: settings, expiresAt: time.Now().Add(ttl).UnixNano()})
}
