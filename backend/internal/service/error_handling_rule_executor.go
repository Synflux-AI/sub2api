package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 本文件是错误处理规则的**平台中立执行层**：把决策层（error_handling_rule_decider.go）
// 的结论落成 UpstreamFailoverError 的字段，剩下的交给各 handler 的 failover 循环。
//
// 拆出来的动机（#228）：这一层原先长在 openAIErrorHandlingRuleOverride 里，于是
// gemini / antigravity / grok-media 想接线就得各重写一份。而真正容易漂移的恰恰是这里：
// SafeErrorType/Message 必须无条件填、retry 必须显式带 RuleRetryLimit、passthrough 必须
// 同时设 NextAccountStop 与 ExhaustedAction —— 每一条漂移点都落在「静默退化成换号」或
// 「通用 502 顶掉上游错误」这类看不见的地方。
//
// 平台特有的四件事作为输入注入，而不是留在这里：
//   - BuiltinOwns：内置逻辑独占这条错误的判定（各平台的 xxxBuiltinOwnsError）；
//   - SafeError：从上游错误体里取可安全返回给客户端的类型与消息；
//   - AccountAccounting：内置不换号时要补跑的账号侧记账；
//   - LogDecision：ops_error_logs / 结构化日志的落地。

// errorHandlingRuleExecInput 是执行层的一次提问。用结构体而不是位置参数：
// 里面有三个 bool，位置传参很容易在新增接入点时错位。
type errorHandlingRuleExecInput struct {
	Settings   ErrorHandlingRuleSettings
	Account    *Account
	StatusCode int
	Header     http.Header
	Body       []byte
	ReqModel   string

	// BuiltinOwns 表示这条错误归内置逻辑独占，规则不得抢走。由调用方按平台算好。
	BuiltinOwns bool

	// BuiltinWillFailover 是内置分类的结论。不参与匹配，只决定「规则接管时要替内置
	// 补跑什么」：内置判定不换号时，调用方拿到非 nil 错误就会早退，从而跳过内置的
	// 错误处理链 —— 那条链里的账号记账与透传规则查询必须在这里补。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应）。
	// 合成状态码只用于**匹配**，绝不能写进 ops_error_logs 的顶层 upstream_status_code：
	// 那一列为 NULL 正是「这是传输层失败」的判定依据。由 LogDecision 的实现负责落实。
	SyntheticStatus bool

	// SafeError 从上游错误体里取出可安全返回给客户端的类型与消息（平台特有格式）。
	SafeError func(body []byte) (errType string, message string)

	// AccountAccounting 是内置不换号时要补跑的账号侧记账。可为 nil。
	AccountAccounting func()

	// LogDecision 落地决策日志。第二参是**生效**动作（不是配置动作）。可为 nil。
	LogDecision func(decision errorHandlingRuleDecision, effectiveAction string)
}

// executeErrorHandlingRule 问一次规则引擎，命中就返回规则版的 failover 错误。
//
// handled == false 时调用方必须原样走内置路径 —— 未命中的请求一行行为都不能变。
func executeErrorHandlingRule(c *gin.Context, in errorHandlingRuleExecInput) (*UpstreamFailoverError, bool) {
	account := in.Account
	if account == nil {
		return nil, false
	}
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}

	// 「错误透传规则」优先。内置判定不换号时，本来是由内置错误处理链去问
	// applyErrorPassthroughRule 并直接写响应的；错误处理规则一旦接管就再也走不到
	// 那里，等于把另一个管理台功能无声关掉。两个功能语义重叠（都能「原样返回上游
	// 错误」），且透传规则是更专用、更早存在的那个，所以它匹配上时本引擎让路。
	// 内置要换号的分支不受影响：那条分支上本来就问不到透传规则。
	if !in.BuiltinWillFailover && errorPassthroughRuleMatches(c, account.Platform, in.StatusCode, in.Body) {
		return nil, false
	}

	decision := decideErrorHandlingRuleFrom(errorHandlingRuleDeciderInput{
		Settings:   in.Settings,
		StatusCode: in.StatusCode,
		Body:       in.Body,
		Platform:   account.Platform,
		Opts: errorHandlingRuleDecisionOptions{
			UpstreamLatencyMs: opsUpstreamLatencyMs(c),
		},
		BuiltinOwns: in.BuiltinOwns,
		// Tracker 为 nil：重试预算按账号计，由 handler 的 sameAccountRetryCount 消耗，
		// 不走 request-scoped tracker。决策层看到 nil tracker 会把 retry 降级成
		// failover，所以 retry 分支在下面单独落地，用 ConfiguredAction 而不是
		// EffectiveAction 判断。
		Tracker: nil,
	})
	if !decision.Matched {
		return nil, false
	}

	// 账号侧记账：内置要换号时由调用方在规则之前跑完，规则只接管动作；内置不换号时
	// 那条记账在被跳过的内置错误处理链里，必须在这里补，否则一个稳定报错的账号永远
	// 不会进入冷却，会被一直调度。
	if !in.BuiltinWillFailover && in.AccountAccounting != nil {
		in.AccountAccounting()
	}

	failoverErr := &UpstreamFailoverError{
		StatusCode:      in.StatusCode,
		ResponseBody:    in.Body,
		ResponseHeaders: respHeader.Clone(),
		ErrorRuleID:     decision.RuleID,
		ExhaustedAction: decision.ExhaustedAction,
	}
	// SafeErrorType/Message 三个动作都要填，不能只填 passthrough：
	// exhausted_action=passthrough 的消费点要求这两个字段非空才认，只在 passthrough
	// 动作上填的话，「换号 + 耗尽后原样返回」这条配置会静默退化成通用 502。
	if in.SafeError != nil {
		failoverErr.SafeErrorType, failoverErr.SafeErrorMessage = in.SafeError(in.Body)
	}
	switch decision.ConfiguredAction {
	case ErrorHandlingActionRetry:
		// 同账号重试，预算按账号计。RuleRetryLimit 必须显式带出来：
		// effectiveSameAccountRetryLimit 的基数是 account.GetPoolModeRetryCount()，
		// 非 pool-mode 账号是 0，不带这个字段的话 retry 会静默退化成换号。
		limit := decision.RetryLimit
		failoverErr.RetryableOnSameAccount = true
		failoverErr.RuleRetryLimit = &limit
		failoverErr.NextAccountAction = NextAccountRetry
	case ErrorHandlingActionPassthrough:
		// 立刻把上游错误返回客户端：不重试、不换号。
		failoverErr.NextAccountAction = NextAccountStop
		failoverErr.ExhaustedAction = ErrorHandlingExhaustedActionPassthrough
	default: // ErrorHandlingActionFailover
		failoverErr.NextAccountAction = NextAccountRetry
	}

	if in.LogDecision != nil {
		in.LogDecision(decision, errorHandlingRuleExecEffectiveAction(decision))
	}
	return failoverErr, true
}

// errorHandlingRuleExecEffectiveAction 是执行层**实际执行**的动作。
//
// 不能直接用 decision.EffectiveAction：执行层传 Tracker=nil（重试预算按账号计，由
// handler 的 sameAccountRetryCount 消耗），决策层看到 nil tracker 会把 retry 恒降级成
// failover 并打上 retry_tracker_missing。那个降级对 Anthropic 的 tracker 路径才成立，
// 在这里是假的 —— 直接拿来当 outcome 会让 OpenObserve 里查不到任何规则重试。
//
// 于是这里按执行层真正落下的动作重算：三个动作原样执行，没有降级。
func errorHandlingRuleExecEffectiveAction(decision errorHandlingRuleDecision) string {
	switch decision.ConfiguredAction {
	case ErrorHandlingActionRetry, ErrorHandlingActionPassthrough:
		return decision.ConfiguredAction
	default:
		return ErrorHandlingActionFailover
	}
}

// syntheticTransportRuleBody 把没有 HTTP 响应的传输层错误合成成统一形状的错误体，
// 供各平台的传输层接线点复用。返回 nil 表示序列化失败，调用方应放弃问规则。
//
// 形状统一用 {"error":{"type","message"}}：规则的响应体匹配是子串/JSON 路径判定，
// 各平台的错误体格式差异在这里没有意义，而统一形状让 SafeError 的实现能共用。
func syntheticTransportRuleBody(safeErr string) []byte {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"message": "upstream request failed: " + strings.TrimSpace(safeErr),
		},
	})
	if err != nil {
		return nil
	}
	return body
}
