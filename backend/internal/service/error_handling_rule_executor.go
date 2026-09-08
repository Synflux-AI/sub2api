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
	// 错误处理链 —— 那条链里唯一必须在这里补的是账号侧记账（见下面的
	// AccountAccounting 消费点）。2026-09-08 起规则引擎全链优先于错误透传规则
	// （反转 #228 非目标），透传规则查询不再是「需要补」的东西——规则引擎胜出时
	// 透传规则本来就不该被问到，这里不补它，是有意为之。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应）。
	// 合成状态码只用于**匹配**，绝不能写进 ops_error_logs 的顶层 upstream_status_code：
	// 那一列为 NULL 正是「这是传输层失败」的判定依据。由 LogDecision 的实现负责落实。
	SyntheticStatus bool

	// SemanticEventForwarded 表示本次流已向客户端写出语义内容（keepalive 心跳不算）。
	// 决策层据此把 retry / failover 降级成 passthrough：已提交的流上拼第二条流会产出
	// 重复的 message_start / 重复 error 帧。零值 false 对所有非流式接线点（HTTP 响应
	// 级/传输层级，写响应前就已问过规则）都是正确值——OpenAI 侧的薄包装不传该字段，
	// 行为不变。
	SemanticEventForwarded bool

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

	// 「错误处理规则」优先于「错误透传规则」。两个功能语义重叠（都能「原样返回上游
	// 错误」），而错误处理规则是挂在具体命中规则上的显式动作（含 exhausted_action），
	// 比透传规则那层更具体，所以两者同时命中时本引擎胜出。
	//
	// 没有任何规则命中时（handled=false），调用方原路走到平台自己的
	// applyErrorPassthroughRule 落点，透传规则照旧生效 —— 变的只是「同时命中」这一种碰撞。
	//
	// 历史：#189/#228 早期口径相反（透传规则优先，本处曾有一个让路分支）。2026-09-08 按
	// 项目所有者决定改为规则引擎优先，issue #228 里「透传规则仍优先」的非目标随之作废。

	decision := decideErrorHandlingRuleFrom(errorHandlingRuleDeciderInput{
		Settings:   in.Settings,
		StatusCode: in.StatusCode,
		Body:       in.Body,
		Platform:   account.Platform,
		Opts: errorHandlingRuleDecisionOptions{
			UpstreamLatencyMs:      opsUpstreamLatencyMs(c),
			SemanticEventForwarded: in.SemanticEventForwarded,
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

	// execAction 是这一层实际要落地的动作。默认跟 ConfiguredAction 走（Tracker=nil
	// 时决策层会把 retry 恒降级成 failover 并打上 retry_tracker_missing，那个降级对
	// 这一层是假的，见 errorHandlingRuleExecEffectiveAction 的文档），但
	// semantic_output_started 这一种降级必须原样尊重：它是「已经把语义内容写给
	// 客户端」这一不可逆事实的直接推论，跟 tracker 缺不缺失无关，任何调用方（包括
	// 未来接的流式接线点）传了 SemanticEventForwarded=true 却被这里悄悄按
	// ConfiguredAction 走成 retry/failover，就会在已提交的流上拼第二条流。
	execAction := decision.ConfiguredAction
	if decision.DowngradeReason == errorHandlingRuleDowngradeReasonSemanticOutputStarted {
		execAction = decision.EffectiveAction
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
	switch execAction {
	case ErrorHandlingActionRetry:
		// 同账号重试，预算按账号计。RuleRetryLimit 必须显式带出来：
		// effectiveSameAccountRetryLimit 裸调用 account.GetPoolModeRetryCount() 的话，
		// 账号基数会顶掉这里配的重试预算——非 pool-mode 或未显式配置时基数是默认值 3，
		// 配了 5 次也只会重试 3 次；管理员把 pool_mode_retry_count 显式设成 0 时，
		// retry 会静默退化成换号。
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
// 但 semantic_output_started 这一种降级例外：它不是「tracker 用错」，是「已经把语义
// 内容写给客户端，retry/failover 不安全」，必须如实落地成 passthrough，否则
// ops_error_logs 里的 Kind 会跟上面 execAction 分支实际执行的动作对不上（这里如果
// 还报 retry/failover，跟真正写的 passthrough 帧自相矛盾）。
//
// 于是这里按执行层真正落下的动作重算：三个动作原样执行，只有 tracker 缺失那一种
// 降级被有意忽略。
func errorHandlingRuleExecEffectiveAction(decision errorHandlingRuleDecision) string {
	if decision.DowngradeReason == errorHandlingRuleDowngradeReasonSemanticOutputStarted {
		return decision.EffectiveAction
	}
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
