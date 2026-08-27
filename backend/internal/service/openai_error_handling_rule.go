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
//     规则插进去会把计数算乱；
//   - body-too-large / access-state / request-scoped 容量削峰：这三类由
//     newOpenAIUpstreamFailoverError 算出 Reason / Scope / Stage / ClientStatusCode /
//     ClientMessage / RequestScopedTransient 等**带类型的**字段，下游的
//     IsOpenAIRequestBodyTooLarge()、IsCredentialFailure()、IsOpenAICapacityShed()、
//     ShouldReportAccountScheduleFailure() 全靠它们分流。规则版错误是另起一个
//     UpstreamFailoverError，带不出这些字段，一条宽泛规则（如「413 → 换号」）会把
//     413 的专用文案、凭据失败的归因、容量削峰的「不扣账号健康分」一起清零。
//
// 其余（通用 4xx/5xx、transient processing、传输层错误）一律允许规则覆盖。
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
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, upstreamMsg, upstreamBody) {
		return true
	}
	if isOpenAIRequestScopedCapacityShed(upstreamMsg, upstreamBody) {
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

// openAIErrorHandlingRuleInput 是 OpenAI 执行层的一次提问。用结构体而不是位置参数：
// 里面有三个 int/bool，位置传参很容易在新增接入点时错位。
type openAIErrorHandlingRuleInput struct {
	Account    *Account
	StatusCode int
	Header     http.Header
	Body       []byte
	ReqModel   string

	// BuiltinWillFailover 是内置分类的结论（shouldFailoverOpenAIUpstreamResponse 的
	// 最终值）。不参与匹配，只决定「规则接管时要替内置补跑什么」：内置判定不换号时，
	// 调用方拿到非 nil 错误就会早退，从而跳过 handleErrorResponse /
	// handleOpenAIImagesErrorResponse 这条链 —— 那条链里有两件不能丢的事，见下。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应，喂给引擎
	// 之前合成了 502）。合成状态码只用于**匹配**，绝不能写进 ops_error_logs 的顶层
	// upstream_status_code：那一列为 NULL 正是「这是传输层失败、根本没有 HTTP 响应」
	// 的判定依据（#189 就是靠 `upstream_status_code IS NULL` 把那 128 条捞出来的）。
	SyntheticStatus bool
}

// openAIErrorHandlingRuleOverride 问一次规则引擎，命中就返回规则版的 failover 错误。
//
// handled == false 时调用方必须原样走内置路径 —— 未命中的请求一行行为都不能变。
func (s *OpenAIGatewayService) openAIErrorHandlingRuleOverride(
	ctx context.Context,
	c *gin.Context,
	in openAIErrorHandlingRuleInput,
) (*UpstreamFailoverError, bool) {
	account := in.Account
	statusCode := in.StatusCode
	respBody := in.Body
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}

	settings, active := s.openAIErrorHandlingRulesActive(ctx, account)
	if !active {
		return nil, false
	}

	// 「错误透传规则」优先。内置判定不换号时，本来是由 handleErrorResponse 一类的链
	// 去问 applyErrorPassthroughRule 并直接写响应的；错误处理规则一旦接管就再也走不到
	// 那里，等于把另一个管理台功能无声关掉。两个功能语义重叠（都能「原样返回上游错误」），
	// 且透传规则是更专用、更早存在的那个，所以它匹配上时本引擎让路。
	// 内置要换号的分支不受影响：那条分支上本来就问不到透传规则。
	if !in.BuiltinWillFailover && openAIErrorPassthroughRuleMatches(c, account.Platform, statusCode, respBody) {
		return nil, false
	}

	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
	decision := decideErrorHandlingRuleFrom(errorHandlingRuleDeciderInput{
		Settings:   settings,
		StatusCode: statusCode,
		Body:       respBody,
		Platform:   account.Platform,
		Opts: errorHandlingRuleDecisionOptions{
			UpstreamLatencyMs: opsUpstreamLatencyMs(c),
		},
		BuiltinOwns: openAIBuiltinOwnsError(statusCode, upstreamMsg, respBody, account),
		// Tracker 为 nil：OpenAI 侧的重试预算按账号计，由 handler 的
		// sameAccountRetryCount 消耗，不走 request-scoped tracker。决策层看到 nil
		// tracker 会把 retry 降级成 failover，所以 retry 分支在下面单独落地，
		// 用 ConfiguredAction 而不是 EffectiveAction 判断。
		Tracker: nil,
	})
	if !decision.Matched {
		return nil, false
	}

	// 账号侧记账：内置要换号时由调用方在规则之前跑完（handleFailoverSideEffects /
	// handleOpenAIAccountUpstreamError），规则只接管动作；内置不换号时那条记账在
	// 被跳过的 handleErrorResponse 链里，必须在这里补，否则一个稳定报错的账号
	// 永远不会进入冷却，会被一直调度。
	if !in.BuiltinWillFailover {
		s.handleOpenAIAccountUpstreamError(ctx, account, statusCode, respHeader, respBody, in.ReqModel)
	}

	failoverErr := &UpstreamFailoverError{
		StatusCode:      statusCode,
		ResponseBody:    respBody,
		ResponseHeaders: respHeader.Clone(),
		ErrorRuleID:     decision.RuleID,
		ExhaustedAction: decision.ExhaustedAction,
	}
	// SafeErrorType/Message 三个动作都要填，不能只填 passthrough：
	// exhausted_action=passthrough 的消费点（两个 handleFailoverExhausted）要求这两个
	// 字段非空才认，只在 passthrough 动作上填的话，「换号 + 耗尽后原样返回」这条配置
	// 会静默退化成通用 502。Anthropic 侧的 errorHandlingRuleFailover 本来就是无条件填的。
	failoverErr.SafeErrorType, failoverErr.SafeErrorMessage = safeOpenAIError(respBody)
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

	s.logOpenAIErrorHandlingRuleDecision(ctx, c, account, in, decision)
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
	failoverErr, handled := s.openAIErrorHandlingRuleOverride(ctx, c, openAIErrorHandlingRuleInput{
		Account:    account,
		StatusCode: openAITransportRuleSyntheticStatus,
		Header:     http.Header{},
		Body:       body,
		// 传输层失败上，内置只有「一律 failover」一种意见，没有「本地写响应」那条链，
		// 所以按 BuiltinWillFailover=true 传：不必替内置补记账，也不该给透传规则让路
		// （透传规则匹配的是真实的上游响应，这里的 502 是合成的）。
		BuiltinWillFailover: true,
		SyntheticStatus:     true,
	})
	if !handled {
		return nil
	}
	return failoverErr
}

// openAIErrorHandlingRuleEffectiveAction 是 OpenAI 执行层**实际执行**的动作。
//
// 不能直接用 decision.EffectiveAction：OpenAI 侧传 Tracker=nil（重试预算按账号计，
// 由 handler 的 sameAccountRetryCount 消耗），决策层看到 nil tracker 会把 retry 恒
// 降级成 failover 并打上 retry_tracker_missing。那个降级对 Anthropic 才成立，在这里
// 是假的 —— 直接拿来当 outcome 会让 OpenObserve 里查不到任何 OpenAI 的规则重试。
//
// 于是这里按执行层真正落下的动作重算：三个动作原样执行，没有降级。
func openAIErrorHandlingRuleEffectiveAction(decision errorHandlingRuleDecision) string {
	switch decision.ConfiguredAction {
	case ErrorHandlingActionRetry, ErrorHandlingActionPassthrough:
		return decision.ConfiguredAction
	default:
		return ErrorHandlingActionFailover
	}
}

// logOpenAIErrorHandlingRuleDecision 与 Anthropic 侧的 logErrorHandlingRuleDecision
// 口径一致。Kind 必须是 "error_handling_rule_" + **生效**动作（不是配置动作）：排查时
// 判断「引擎有没有被绕过」全靠 upstream_errors 里有没有这个前缀，而两个平台的
// outcome 字段必须能直接对比，否则跨平台查询会把配置值和生效值混在一起。
func (s *OpenAIGatewayService) logOpenAIErrorHandlingRuleDecision(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	in openAIErrorHandlingRuleInput,
	decision errorHandlingRuleDecision,
) {
	statusCode := in.StatusCode
	respBody := in.Body
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}
	effectiveAction := openAIErrorHandlingRuleEffectiveAction(decision)
	upstreamMsg := extractUpstreamErrorMessage(respBody)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		upstreamDetail = truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
	}

	// ops_error_logs 的顶层列（upstream_status_code / message）必须在这里补一次：
	// 规则命中会让调用方跳过 handleErrorResponse 一类的内置错误处理链，而
	// setOpsUpstreamError 原先只在那条链里调。不补的话，被规则接管的请求在
	// ops_error_logs 里 upstream_status_code 是 NULL —— 正是 #189 用来定案的那几列。
	//
	// 但**合成**状态码不能写进去：传输层失败根本没有 HTTP 响应，那一列为 NULL 正是
	// 「这是传输层失败」的判定依据（#189 就是靠 `upstream_status_code IS NULL` 把那
	// 128 条捞出来的）。传 0 让 setOpsUpstreamError 只落 message、不动状态码。
	opsStatusCode := statusCode
	if in.SyntheticStatus {
		opsStatusCode = 0
	}
	setOpsUpstreamError(c, opsStatusCode, sanitizeUpstreamErrorMessage(strings.TrimSpace(upstreamMsg)), upstreamDetail)

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: opsStatusCode,
		UpstreamRequestID:  respHeader.Get("x-request-id"),
		Kind:               "error_handling_rule_" + effectiveAction,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})

	gatewayLog(ctx).Warn("error_handling_rule_matched",
		zap.String("rule_id", decision.RuleID),
		zap.String("rule_name", decision.RuleName),
		zap.String("rule_action", decision.ConfiguredAction),
		zap.String("outcome", effectiveAction),
		zap.String("exhausted_action", decision.ExhaustedAction),
		zap.Int("upstream_status_code", opsStatusCode),
		// 合成 502 只用于匹配，单独记一条，免得排查时把它当成真实上游状态码。
		zap.Bool("synthetic_status", in.SyntheticStatus),
		zap.Int("matched_status_code", statusCode),
		zap.String("upstream_request_id", respHeader.Get("x-request-id")),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.String("upstream_model", in.ReqModel),
		zap.Int("rule_retry_limit", decision.RetryLimit),
		zap.String("upstream_message", truncateString(sanitizeUpstreamErrorMessage(upstreamMsg), 256)),
	)
}
