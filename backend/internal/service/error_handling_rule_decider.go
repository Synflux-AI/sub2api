package service

import (
	"net/http"
	"time"
)

// 本文件是错误处理规则的**平台中立决策层**：只匹配规则、消耗所选规则的重试预算、
// 算出生效动作。响应写出、sleep、账号副作用一律留给调用方 —— 那些是平台特有的。
//
// 拆出来的动机（#189）：这一层原先挂在 *GatewayService 上，于是 OpenAI 侧
// （*OpenAIGatewayService）用不了，只能重写一份。而真正有价值的恰恰是这里的细节：
// 四个降级分支、consume vs consumeForRule 两套预算、RetryDelay 被 maxRetryElapsed
// 夹逼 —— 重写必然漂移，且漂移点全在「重复扣费 / 半截响应」这类不可逆的地方。
//
// 平台特有的两件事作为输入注入，而不是留在这里：
//   - 账号是否适用规则（isErrorHandlingRuleAccount 之类）：调用方先判断；
//   - 「内置逻辑独占这条错误」（Anthropic 是 Thinking 签名，OpenAI 是 cyber_policy /
//     context-window / OAuth 429 状态机）：调用方算好，用 BuiltinOwns 传进来。

type errorHandlingRuleTracker struct {
	ruleID  string
	retries int

	retriesByRule map[string]int

	// evaluated 记录最近一次被规则引擎评估过的响应对象。转发主循环里有若干条
	// 从 400 分支内部 break 出去的路径，它们带出的响应从没经过任何接入点，
	// 循环后的兜底靠这个指针区分「已评估过」和「漏网的」，避免重复评估。
	evaluated *http.Response
}

func (t *errorHandlingRuleTracker) consume(ruleID string, retryLimit int) bool {
	if t.ruleID != ruleID {
		t.ruleID = ruleID
		t.retries = 0
	}
	if t.retries >= retryLimit {
		return false
	}
	t.retries++
	return true
}

func (t *errorHandlingRuleTracker) consumeForRule(ruleID string, retryLimit int) bool {
	if t.retriesByRule == nil {
		t.retriesByRule = make(map[string]int)
	}
	if t.retriesByRule[ruleID] >= retryLimit {
		return false
	}
	t.retriesByRule[ruleID]++
	return true
}

func (t *errorHandlingRuleTracker) retryCount(ruleID string, independent bool) int {
	if t == nil {
		return 0
	}
	if !independent {
		return t.retries
	}
	if t.retriesByRule == nil {
		return 0
	}
	return t.retriesByRule[ruleID]
}

func (t *errorHandlingRuleTracker) markEvaluated(resp *http.Response) {
	t.evaluated = resp
}

func (t *errorHandlingRuleTracker) alreadyEvaluated(resp *http.Response) bool {
	return t.evaluated != nil && t.evaluated == resp
}

type errorHandlingRuleOutcome int

const (
	errorHandlingRuleOutcomeNone errorHandlingRuleOutcome = iota
	errorHandlingRuleOutcomeRetry
	errorHandlingRuleOutcomeDone
)

type errorHandlingRuleDecision struct {
	Matched          bool
	RuleID           string
	RuleName         string
	ConfiguredAction string
	EffectiveAction  string
	RetryDelay       time.Duration
	ExhaustedAction  string
	DowngradeReason  string
	RetryUsed        int
	RetryLimit       int
	RetryElapsed     time.Duration
}

type errorHandlingRuleDecisionOptions struct {
	Attempt                int
	RetryStart             time.Time
	IgnoreRetryElapsed     bool
	SemanticEventForwarded bool
	IndependentRetryBudget bool
}

// errorHandlingRuleDeciderInput 是决策层的全部输入。没有 gin.Context、没有
// *http.Response、没有任何平台的错误体格式 —— 加进来就说明该判断放错了层。
type errorHandlingRuleDeciderInput struct {
	Settings   ErrorHandlingRuleSettings
	Tracker    *errorHandlingRuleTracker
	StatusCode int
	Body       []byte
	Opts       errorHandlingRuleDecisionOptions

	// BuiltinOwns 表示这条错误归内置逻辑独占，规则不得抢走。由调用方按平台算好。
	BuiltinOwns bool
}

// decideErrorHandlingRuleFrom only matches rules, consumes the selected rule's
// retry budget, and computes the effective action. Response writes, sleeps, and
// account side effects are deliberately left to the caller.
func decideErrorHandlingRuleFrom(in errorHandlingRuleDeciderInput) errorHandlingRuleDecision {
	if in.BuiltinOwns || !in.Settings.Enabled {
		return errorHandlingRuleDecision{}
	}
	rule := matchErrorHandlingRule(in.Settings.Rules, in.StatusCode, in.Body)
	if rule == nil {
		return errorHandlingRuleDecision{}
	}
	retryLimit := rule.RetryLimit(in.Settings.DefaultRetryCount)

	tracker := in.Tracker
	opts := in.Opts
	decision := errorHandlingRuleDecision{
		Matched:          true,
		RuleID:           rule.ID,
		RuleName:         rule.Name,
		ConfiguredAction: rule.Action,
		EffectiveAction:  rule.Action,
		ExhaustedAction:  rule.ExhaustedAction,
		RetryLimit:       retryLimit,
	}
	if !opts.RetryStart.IsZero() {
		decision.RetryElapsed = time.Since(opts.RetryStart)
	}

	if opts.SemanticEventForwarded &&
		(rule.Action == ErrorHandlingActionRetry || rule.Action == ErrorHandlingActionFailover) {
		decision.EffectiveAction = ErrorHandlingActionPassthrough
		decision.DowngradeReason = "semantic_output_started"
		return decision
	}
	if rule.Action != ErrorHandlingActionRetry {
		return decision
	}
	if tracker == nil {
		decision.EffectiveAction = ErrorHandlingActionFailover
		decision.DowngradeReason = "retry_tracker_missing"
		return decision
	}

	consumeRetry := tracker.consume
	if opts.IndependentRetryBudget {
		consumeRetry = tracker.consumeForRule
	}
	switch {
	case !consumeRetry(rule.ID, retryLimit):
		decision.EffectiveAction = ErrorHandlingActionFailover
		decision.DowngradeReason = "retry_budget_exhausted"
		decision.RetryUsed = retryLimit
	case !opts.IgnoreRetryElapsed && decision.RetryElapsed >= maxRetryElapsed:
		decision.EffectiveAction = ErrorHandlingActionFailover
		decision.DowngradeReason = "retry_window_elapsed"
		decision.RetryUsed = tracker.retryCount(rule.ID, opts.IndependentRetryBudget)
	case opts.Attempt >= maxRetryAttempts:
		decision.EffectiveAction = ErrorHandlingActionFailover
		decision.DowngradeReason = "max_attempts_reached"
		decision.RetryUsed = tracker.retryCount(rule.ID, opts.IndependentRetryBudget)
	default:
		decision.RetryUsed = tracker.retryCount(rule.ID, opts.IndependentRetryBudget)
		decision.RetryDelay = retryBackoffDelay(opts.Attempt)
		if !opts.IgnoreRetryElapsed {
			if remaining := maxRetryElapsed - decision.RetryElapsed; decision.RetryDelay > remaining {
				decision.RetryDelay = remaining
			}
		}
	}
	return decision
}
