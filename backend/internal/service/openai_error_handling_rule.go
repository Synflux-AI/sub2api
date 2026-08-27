package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 错误处理规则的 OpenAI 执行层。
//
// 决策是平台中立的（error_handling_rule_decider.go），执行是平台特有的：Anthropic
// 侧把决策落成「写响应 / sleep / 换号」，OpenAI 侧的转发主循环在 handler 里，所以
// 这里只把决策映射到既有的 UpstreamFailoverError 字段，剩下的交给 handler 的
// failover 循环。
//
// 与 Anthropic 侧的一处已知语义差异（刻意为之）：
//   - Anthropic：规则重试预算按 **规则** 计（errorHandlingRuleTracker，键是 ruleID）；
//   - OpenAI：按 **账号** 计（handler 里现成的 sameAccountRetryCount[account.ID]）。
//
// 要对齐就得把 request-scoped 的 tracker 塞进 OpenAI handler 里 18 处 failover 循环，
// 那是 18 处改动换一个语义对齐。第一版用现成通道，差异写在这里和 issue #189 里。

// openAITransportRuleSyntheticStatus 是传输层错误喂给规则引擎时用的合成状态码。
//
// 传输层失败没有 HTTP 响应（2026-08-26 那 128 条 lost-ping 在 ops_error_logs 里
// upstream_status_code 是 NULL），引擎又只看状态码+响应体，所以这里合成一个。
// 仓库已有先例：Anthropic passthrough 的「流中断」规则同样合成 502 + JSON body。
const openAITransportRuleSyntheticStatus = http.StatusBadGateway

// openAIBuiltinOwnsError 判断这条上游错误是否归内置逻辑独占，规则不得抢走。
//
// 判据是「规则动作对这类错误是否必然错误或有害」：
//   - cyber_policy：request-scoped，换号/重试都是空耗，还误伤凭据；
//   - context window：确定性错误，换任何号都复现；
//   - OAuth 账号的 429：ShouldStopOpenAIOAuth429Failover 是跨 switch 的计数状态机，
//     规则插进去会把计数算乱。
//
// 其余（通用 4xx/5xx、transient processing、access-state、body-too-large、传输层
// 错误）一律允许规则覆盖。
func openAIBuiltinOwnsError(statusCode int, upstreamMsg string, upstreamBody []byte, account *Account) bool {
	if hit, _, _ := detectOpenAICyberPolicy(upstreamBody); hit {
		return true
	}
	if isOpenAIContextWindowError(upstreamMsg, upstreamBody) {
		return true
	}
	if statusCode == http.StatusTooManyRequests && account != nil && account.IsOpenAIOAuthLike() {
		return true
	}
	return false
}

// safeOpenAIError 从上游错误体里取出可安全返回给客户端的类型与消息。
// 对齐 Anthropic 侧的 safeAnthropicError：passthrough 动作要把上游错误原样交给
// 客户端，但不能把上游的内部细节（含可能的凭据片段）直接透出去。
func safeOpenAIError(body []byte) (string, string) {
	var payload struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != nil {
		errType := strings.TrimSpace(payload.Error.Type)
		message := sanitizeUpstreamErrorMessage(strings.TrimSpace(payload.Error.Message))
		if errType != "" && message != "" {
			return errType, message
		}
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if message == "" {
		message = "Upstream request failed"
	}
	return "upstream_error", message
}

// openAIErrorHandlingRulesActive 是热路径早退出：没配规则、没勾 openai、账号不是
// OpenAI 平台时，一次配置读取以外什么都不做。
//
// 与 Anthropic 侧不同，这里**不卡账号类型**：isErrorHandlingRuleAccount 除平台外还
// 要求 Type == AccountTypeAPIKey，那是当年用账号类型给 OAuth 做的粗粒度兜底。
// OAuth 的真正风险点是 429 状态机，已经列进 openAIBuiltinOwnsError 独占，不必再用
// 账号类型二次设限。
func (s *OpenAIGatewayService) openAIErrorHandlingRulesActive(ctx context.Context, account *Account) (ErrorHandlingRuleSettings, bool) {
	if s == nil || s.settingService == nil || account == nil || account.Platform != PlatformOpenAI {
		return ErrorHandlingRuleSettings{}, false
	}
	settings := s.settingService.GetErrorHandlingRuleSettingsCached(ctx)
	if !settings.Enabled || !HasEnabledErrorHandlingRuleForPlatform(settings.Rules, account.Platform) {
		return ErrorHandlingRuleSettings{}, false
	}
	return settings, true
}

// openAIErrorHandlingRuleOverride 问一次规则引擎，命中就返回规则版的 failover 错误。
//
// handled == false 时调用方必须原样走内置路径 —— 未命中的请求一行行为都不能变。
func (s *OpenAIGatewayService) openAIErrorHandlingRuleOverride(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	statusCode int,
	respHeader http.Header,
	respBody []byte,
	reqModel string,
) (*UpstreamFailoverError, bool) {
	settings, active := s.openAIErrorHandlingRulesActive(ctx, account)
	if !active {
		return nil, false
	}

	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
	decision := decideErrorHandlingRuleFrom(errorHandlingRuleDeciderInput{
		Settings:          settings,
		StatusCode:        statusCode,
		Body:              respBody,
		Platform:          account.Platform,
		UpstreamLatencyMs: opsUpstreamLatencyMs(c),
		BuiltinOwns:       openAIBuiltinOwnsError(statusCode, upstreamMsg, respBody, account),
		// Tracker 为 nil：OpenAI 侧的重试预算按账号计，由 handler 的
		// sameAccountRetryCount 消耗，不走 request-scoped tracker。决策层看到 nil
		// tracker 会把 retry 降级成 failover，所以 retry 分支在下面单独落地，
		// 用 ConfiguredAction 而不是 EffectiveAction 判断。
		Tracker: nil,
	})
	if !decision.Matched {
		return nil, false
	}

	s.logOpenAIErrorHandlingRuleDecision(ctx, c, account, statusCode, respHeader, respBody, reqModel, decision)

	failoverErr := &UpstreamFailoverError{
		StatusCode:      statusCode,
		ResponseBody:    respBody,
		ResponseHeaders: respHeader.Clone(),
		ErrorRuleID:     decision.RuleID,
		ExhaustedAction: decision.ExhaustedAction,
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
		failoverErr.SafeErrorType, failoverErr.SafeErrorMessage = safeOpenAIError(respBody)
	default: // ErrorHandlingActionFailover
		failoverErr.NextAccountAction = NextAccountRetry
	}
	return failoverErr, true
}

// openAITransportErrorRuleOverride 把传输层错误（无 HTTP 状态码）合成成 502 + OpenAI
// 形状的错误体后问规则。命中返回规则版错误，未命中返回 nil。
func (s *OpenAIGatewayService) openAITransportErrorRuleOverride(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	safeErr string,
) *UpstreamFailoverError {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"message": "upstream request failed: " + safeErr,
		},
	})
	if err != nil {
		return nil
	}
	failoverErr, handled := s.openAIErrorHandlingRuleOverride(
		ctx, c, account, openAITransportRuleSyntheticStatus, http.Header{}, body, "")
	if !handled {
		return nil
	}
	return failoverErr
}

// logOpenAIErrorHandlingRuleDecision 与 Anthropic 侧的 logErrorHandlingRuleDecision
// 口径一致。Kind 必须是 "error_handling_rule_" + action：排查时判断「引擎有没有被
// 绕过」全靠 upstream_errors 里有没有这个前缀。
func (s *OpenAIGatewayService) logOpenAIErrorHandlingRuleDecision(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	statusCode int,
	respHeader http.Header,
	respBody []byte,
	reqModel string,
	decision errorHandlingRuleDecision,
) {
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusCode,
		UpstreamRequestID:  respHeader.Get("x-request-id"),
		Kind:               "error_handling_rule_" + decision.ConfiguredAction,
		Message:            extractUpstreamErrorMessage(respBody),
	})

	gatewayLog(ctx).Warn("error_handling_rule_matched",
		zap.String("rule_id", decision.RuleID),
		zap.String("rule_name", decision.RuleName),
		zap.String("rule_action", decision.ConfiguredAction),
		zap.String("outcome", decision.ConfiguredAction),
		zap.String("exhausted_action", decision.ExhaustedAction),
		zap.Int("upstream_status_code", statusCode),
		zap.String("upstream_request_id", respHeader.Get("x-request-id")),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.String("upstream_model", reqModel),
		zap.Int("rule_retry_limit", decision.RetryLimit),
		zap.String("upstream_message", truncateString(sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody)), 256)),
	)
}
