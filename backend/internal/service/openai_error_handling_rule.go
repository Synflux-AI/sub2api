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
//
//   - cyber_policy：request-scoped，换号/重试都是空耗，还误伤凭据；
//
//   - context window：确定性错误，换任何号都复现；
//
//   - OAuth 账号的 429：ShouldStopOpenAIOAuth429Failover 是跨 switch 的计数状态机，
//     规则插进去会把计数算乱；
//
//   - body-too-large / access-state / request-scoped 容量削峰：这三类由
//     newOpenAIUpstreamFailoverError 算出 Reason / Scope / Stage / ClientStatusCode /
//     ClientMessage / RequestScopedTransient 等**带类型的**字段，下游的
//     IsOpenAIRequestBodyTooLarge()、IsCredentialFailure()、IsOpenAICapacityShed()、
//     ShouldReportAccountScheduleFailure() 全靠它们分流。规则版错误是另起一个
//     UpstreamFailoverError，带不出这些字段，一条宽泛规则（如「413 → 换号」）会把
//     413 的专用文案、凭据失败的归因、容量削峰的「不扣账号健康分」一起清零。
//
//   - Grok 内容策略拒绝（isGrokContentPolicyRejection）：确定性错误，同一 prompt 在
//     账号池里任何账号上都复现，换号只是空耗上游请求；grok_media.go 侧
//     （grokMediaBuiltinOwnsError）已经把这类错误判给内置独占，文本推理走的是
//     OpenAIGatewayService，早前因为平台闸门挡住 Platform=grok 而从未触达这里，三道
//     闸门打开后必须在这里补一份同样的独占，否则一条宽泛的「403 → 换号」规则会把
//     grokContentPolicyClientMessage 这条专用文案换成规则版的通用耗尽错误。
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
	if account != nil && account.Platform == PlatformGrok && isGrokContentPolicyRejection(statusCode, upstreamBody) {
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

// openAIErrorHandlingRulesActive 是热路径早退出：没配规则、没勾对应平台、账号不是
// isConcreteRequestPlatform 认可的具体平台时，一次配置读取以外什么都不做。
// OpenAIGatewayService 是 openai / grok / kimi / zhipu / deepseek 文本推理的共同
// 宿主，这里放行的是这一整组具体平台，不再只认 PlatformOpenAI。
//
// 与 Anthropic 侧不同，这里**不卡账号类型**：isErrorHandlingRuleAccount 对
// PlatformAnthropic 额外要求 Type == AccountTypeAPIKey，那是当年用账号类型给 OAuth
// 做的粗粒度兜底。OAuth 的真正风险点是各平台的 429 状态机，已经列进各自的
// BuiltinOwns 独占，不必再用账号类型二次设限。
func (s *OpenAIGatewayService) openAIErrorHandlingRulesActive(ctx context.Context, account *Account) (ErrorHandlingRuleSettings, bool) {
	if s == nil || s.settingService == nil || account == nil || !isConcreteRequestPlatform(account.Platform) {
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
	// handleOpenAIImagesErrorResponse 这条链 —— 那条链里唯一必须补的是账号侧记账
	// （executor 层 AccountAccounting 消费点）。2026-09-08 起规则引擎全链优先于错误
	// 透传规则，这条链里的透传规则查询不再需要补——规则引擎胜出时它本来就不该被
	// 问到。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应，喂给引擎
	// 之前合成了 502）。合成状态码只用于**匹配**，绝不能写进 ops_error_logs 的顶层
	// upstream_status_code：那一列为 NULL 正是「这是传输层失败、根本没有 HTTP 响应」
	// 的判定依据（#189 就是靠 `upstream_status_code IS NULL` 把那 128 条捞出来的）。
	SyntheticStatus bool

	// SemanticEventForwarded 表示当前流已经向客户端写出语义内容。共享执行层据此
	// 把 retry / failover 降级成安全的 passthrough 终态。
	SemanticEventForwarded bool
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

	return executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:               settings,
		Account:                account,
		StatusCode:             statusCode,
		Header:                 respHeader,
		Body:                   respBody,
		BuiltinOwns:            openAIBuiltinOwnsError(statusCode, sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody))), respBody, account),
		BuiltinWillFailover:    in.BuiltinWillFailover,
		SyntheticStatus:        in.SyntheticStatus,
		SemanticEventForwarded: in.SemanticEventForwarded,
		SafeError:              safeOpenAIError,
		AccountAccounting: func() {
			if account.Platform == PlatformGrok {
				s.handleGrokAccountUpstreamError(withGrokTeamRateLimitModel(ctx, in.ReqModel), account, statusCode, respHeader, respBody)
				return
			}
			s.handleOpenAIAccountUpstreamError(ctx, account, statusCode, respHeader, respBody, in.ReqModel)
		},
		LogDecision: func(decision errorHandlingRuleDecision, effectiveAction string) {
			s.logOpenAIErrorHandlingRuleDecision(ctx, c, account, in, decision, effectiveAction)
		},
	})
}

// openAITransportErrorRuleOverride 把没有完整可用 HTTP 响应的传输层错误合成成
// 502 + OpenAI 形状的错误体后问规则。命中返回规则版错误，未命中返回 nil。
func (s *OpenAIGatewayService) openAITransportErrorRuleOverride(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	safeErr string,
) *UpstreamFailoverError {
	body := syntheticTransportRuleBody(safeErr)
	if body == nil {
		return nil
	}
	failoverErr, handled := s.openAIErrorHandlingRuleOverride(ctx, c, openAIErrorHandlingRuleInput{
		Account:    account,
		StatusCode: openAITransportRuleSyntheticStatus,
		Header:     http.Header{},
		Body:       body,
		// 传输层失败上，内置只有「一律 failover」一种意见，没有「本地写响应」那条链，
		// 所以按 BuiltinWillFailover=true 传：不必替内置补记账。错误透传规则在这条
		// 路径上也问不到：透传规则匹配的是真实的上游响应，这里的 502 是合成的（与
		// "规则引擎优先于透传规则"的口径无关）。
		BuiltinWillFailover: true,
		SyntheticStatus:     true,
	})
	if !handled {
		return nil
	}
	return failoverErr
}

type openAIStreamErrorHandlingRuleInput struct {
	Account                *Account
	Header                 http.Header
	Payload                []byte
	Message                string
	ReqModel               string
	StatusCode             int
	SyntheticStatus        bool
	SemanticEventForwarded bool
}

func (s *OpenAIGatewayService) openAIStreamErrorHandlingRuleOverride(
	ctx context.Context,
	c *gin.Context,
	in openAIStreamErrorHandlingRuleInput,
) *UpstreamFailoverError {
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(in.Message))
	if message == "" {
		message = "OpenAI stream failed"
	}
	body := in.Payload
	statusCode := in.StatusCode
	if in.SyntheticStatus {
		statusCode = openAITransportRuleSyntheticStatus
		body = syntheticTransportRuleBody(message)
	} else if statusCode == 0 {
		statusCode = openAIStreamFailedEventSemanticStatus(body, message)
	}
	if len(body) == 0 {
		body = syntheticTransportRuleBody(message)
	}
	failoverErr, handled := s.openAIErrorHandlingRuleOverride(ctx, c, openAIErrorHandlingRuleInput{
		Account:                in.Account,
		StatusCode:             statusCode,
		Header:                 in.Header,
		Body:                   body,
		ReqModel:               in.ReqModel,
		BuiltinWillFailover:    false,
		SyntheticStatus:        in.SyntheticStatus,
		SemanticEventForwarded: in.SemanticEventForwarded,
	})
	if !handled {
		return nil
	}
	failoverErr.SafeToFailoverAfterWrite = !in.SemanticEventForwarded
	failoverErr.SafeErrorType = "upstream_error"
	if statusCode == http.StatusTooManyRequests {
		failoverErr.SafeErrorType = "rate_limit_error"
	}
	failoverErr.SafeErrorMessage = message
	return failoverErr
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
	effectiveAction string,
) {
	statusCode := in.StatusCode
	respBody := in.Body
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}
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
