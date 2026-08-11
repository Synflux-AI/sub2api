package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	ID              string   `json:"id"`
	Name            string   `json:"name"`
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

func matchErrorHandlingRule(rules []ErrorHandlingRule, statusCode int, respBody []byte) *ErrorHandlingRule {
	if len(rules) == 0 {
		return nil
	}

	var bodyLower string
	bodyLowered := false
	for i := range rules {
		rule := &rules[i]
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
		// 空 ID 会让 errorHandlingRuleTracker 把不同规则当成同一条（都是 ""），
		// 重试计数被共用。补一个确定性的占位 ID：不用随机值，否则每次读配置
		// 都变一次，日志里同一条规则的 rule_id 对不上。
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" {
			rule.ID = fmt.Sprintf("rule-%d", i+1)
		}
	}
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
