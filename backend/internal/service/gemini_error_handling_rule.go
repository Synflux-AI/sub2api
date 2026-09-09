package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// 错误处理规则的 Gemini 执行层。
//
// 决策是平台中立的（error_handling_rule_decider.go），执行是平台特有的：Gemini 的
// 三条转发链（Forward / ForwardNative / ForwardAsChatCompletions）都挂在同一个
// *GeminiMessagesCompatService 接收者上，所以这里不需要像 OpenAI/Anthropic 那样
// 为多个 service 类型各写一份——一个方法三处复用即可，避免逻辑漂移。
//
// 与 OpenAI 侧一处关键的语义差异（#228 接线时必须留意）：
//   - OpenAI：账号记账在接线点**之后**才跑（内置不换号时才补），所以
//     BuiltinWillFailover 必须传真实结论，执行层才能正确决定要不要替内置补跑
//     AccountAccounting；
//   - Gemini：账号记账（handleGeminiUpstreamError）在接线点**之前**就已经跑完（无论
//     内置最终判不判定 failover），但内置判定不 failover 的分支仍会继续走到
//     writeGeminiMappedError/writeGeminiChatCompletionsMappedError，那里会问
//     applyErrorPassthroughRule。
//
// 所以 Gemini 侧接线必须传：
//   - BuiltinWillFailover: shouldFailoverGeminiUpstreamError(statusCode) 的真实结论
//     （不能硬编码 true）——它只决定「规则接管时要不要替内置补跑账号记账」
//     （AccountAccounting），跟错误透传规则的优先级无关：2026-09-08 起规则引擎
//     全链优先于透传规则，不再有"让路"（见 error_handling_rule_executor.go）；
//   - AccountAccounting: nil ——记账已经跑完，执行层的 nil 保护会跳过重复扣分。

// geminiBuiltinOwnsError 判断这条上游错误是否归内置逻辑独占，规则不得抢走。
//
// 三条转发链在这一点上的状况并不对称，逐条核对（#228 task-7 fix round 1）：
//   - Forward / ForwardNative：CheckErrorPolicy 的 switch（ErrorPolicySkipped /
//     ErrorPolicyMatched / ErrorPolicyTempUnscheduled）与 isGoogleProjectConfigError
//     的 400 特判，都已经在接线点之前物理 return，规则引擎压根碰不到这两类错误。
//     这里对这两条链而言是双重保险——就算判了 true，也不会改变已经在更早处 return
//     的结果。
//   - ForwardAsChatCompletions：没有上述任何一类早退分支。它唯一涉及的
//     checkErrorPolicyInLoop 只决定同号重试循环要不要 break，从不短路错误处理路径，
//     所以该链会带着未经拦截的 400 一路走到这个判断点。这里如果恒为 false，一条形如
//     「400 → failover」的管理台规则就能抢下一个确定性的 Google 项目配置错误——这种
//     错误换任何账号都复现，被规则接管意味着整个账号池会被无谓地打一遍换号，是纯粹的
//     行为退化。所以这个判断在这条链上是**承重的**，不能省，也不能只留个恒 false 的
//     占位。
//
// 检测口径必须与 Forward 里 msg400 的构造方式逐字一致（ToLower + TrimSpace 作用于
// extractUpstreamErrorMessage，不经过 sanitizeUpstreamErrorMessage），否则两处判定
// 可能在边界样本上不一致，调用方已按这个口径准备 upstreamMsg。
//
// 保持窄：只处理这一种确定性配置错误，不要顺手加别的条件——新分支需要新的证据。
func geminiBuiltinOwnsError(statusCode int, upstreamMsg string, _ []byte) bool {
	return statusCode == http.StatusBadRequest && isGoogleProjectConfigError(upstreamMsg)
}

// safeGeminiError 从上游错误体里取出可安全返回给客户端的类型与消息。
// Google 原生错误形状是 {"error":{"code":N,"message":"...","status":"..."}}，
// errType 取 status 字段（如 RESOURCE_EXHAUSTED），缺失时退回 "upstream_error"。
func safeGeminiError(body []byte) (string, string) {
	var payload struct {
		Error *struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != nil {
		errType := strings.TrimSpace(payload.Error.Status)
		message := sanitizeUpstreamErrorMessage(strings.TrimSpace(payload.Error.Message))
		if message != "" {
			if errType == "" {
				errType = "upstream_error"
			}
			return errType, message
		}
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if message == "" {
		message = "Upstream request failed"
	}
	return "upstream_error", message
}

// geminiErrorHandlingRulesActive 是热路径早退出：没配规则、没勾 gemini、账号不是
// Gemini 平台时，一次配置读取以外什么都不做。
//
// 不卡账号类型：Gemini 没有 Anthropic 侧那种「OAuth 账号靠账号类型兜底排除」的历史
// 包袱，OAuth 的真正风险点（如果有）应该进 geminiBuiltinOwnsError，不该用账号类型
// 二次设限。
func (s *GeminiMessagesCompatService) geminiErrorHandlingRulesActive(ctx context.Context, account *Account) (ErrorHandlingRuleSettings, bool) {
	if s == nil || s.settingService == nil || account == nil || account.Platform != PlatformGemini {
		return ErrorHandlingRuleSettings{}, false
	}
	settings := s.settingService.GetErrorHandlingRuleSettingsCached(ctx)
	if !settings.Enabled || !HasEnabledErrorHandlingRuleForPlatform(settings.Rules, account.Platform) {
		return ErrorHandlingRuleSettings{}, false
	}
	return settings, true
}

// geminiErrorHandlingRuleInput 是 Gemini 执行层的一次提问。用结构体而不是位置参数：
// 里面有多个 int/bool，位置传参很容易在新增接入点时错位。
type geminiErrorHandlingRuleInput struct {
	Account    *Account
	StatusCode int
	Header     http.Header
	Body       []byte
	ReqModel   string

	// BuiltinWillFailover 必须传真实的内置分类结论
	// （shouldFailoverGeminiUpstreamError(StatusCode)），不能硬编码 true：执行层只用
	// 它决定「规则接管时要不要替内置补跑账号记账」（AccountAccounting）。Gemini 侧
	// 记账（handleGeminiUpstreamError）已经在接线点之前跑完。与错误透传规则的优先级
	// 无关：2026-09-08 起规则引擎全链优先，不再有"让路"。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应）。只用于
	// **匹配**，绝不能写进 ops_error_logs 顶层的 upstream_status_code：那一列为 NULL
	// 正是「这是传输层失败」的判定依据。由 logGeminiErrorHandlingRuleDecision 负责落实。
	SyntheticStatus bool

	// SemanticEventForwarded 表示当前流已经向客户端写出语义内容。规则命中 retry /
	// failover 时，共享执行层会把动作降级为 passthrough，避免在已提交的流后拼接
	// 另一个账号的响应。
	SemanticEventForwarded bool
}

// geminiErrorHandlingRuleOverride 问一次规则引擎，命中就返回规则版的 failover 错误。
//
// handled == false 时调用方必须原样走内置路径——未命中的请求一行行为都不能变。
// Forward / ForwardNative / ForwardAsChatCompletions 三条转发链共用这一个方法。
func (s *GeminiMessagesCompatService) geminiErrorHandlingRuleOverride(
	ctx context.Context,
	c *gin.Context,
	in geminiErrorHandlingRuleInput,
) (*UpstreamFailoverError, bool) {
	account := in.Account
	statusCode := in.StatusCode
	respBody := in.Body
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}

	settings, active := s.geminiErrorHandlingRulesActive(ctx, account)
	if !active {
		return nil, false
	}

	// 与 Forward 里 msg400 的构造方式逐字一致：ToLower + TrimSpace 作用于
	// extractUpstreamErrorMessage 的结果，不经过 sanitizeUpstreamErrorMessage。
	// geminiBuiltinOwnsError 靠这个口径识别确定性的 Google 项目配置类 400
	// （见该函数的文档注释）。
	lowerMsg := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))

	return executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:               settings,
		Account:                account,
		StatusCode:             statusCode,
		Header:                 respHeader,
		Body:                   respBody,
		BuiltinOwns:            geminiBuiltinOwnsError(statusCode, lowerMsg, respBody),
		BuiltinWillFailover:    in.BuiltinWillFailover,
		SyntheticStatus:        in.SyntheticStatus,
		SemanticEventForwarded: in.SemanticEventForwarded,
		SafeError:              safeGeminiError,
		// 记账已在接线点之前由 handleGeminiUpstreamError 跑完，不再补跑，否则会重复
		// 扣账号健康分。
		AccountAccounting: nil,
		LogDecision: func(decision errorHandlingRuleDecision, effectiveAction string) {
			s.logGeminiErrorHandlingRuleDecision(ctx, c, account, in, decision, effectiveAction)
		},
	})
}

// geminiTransportRuleSyntheticStatus 是传输层错误喂给规则引擎时用的合成状态码。
//
// 传输层失败没有 HTTP 响应，引擎又只看状态码+响应体，所以这里合成一个。与 OpenAI
// 侧 openAITransportRuleSyntheticStatus（openai_error_handling_rule.go）同一先例。
const geminiTransportRuleSyntheticStatus = http.StatusBadGateway

// geminiTransportErrorRuleOverride 把没有完整可用 HTTP 响应的传输层错误合成成 502 +
// 统一形状的错误体（syntheticTransportRuleBody）后问规则。命中返回规则版的
// failover 错误，未命中返回 nil。Forward / ForwardNative / ForwardAsChatCompletions
// 三条转发链在各自的重试循环耗尽后调用这里（#228 task-10）——调用方必须先排除客户端
// 断连（ctx.Err() != nil）：那种情况下 upstream 请求已经打出去，没人会读响应，规则
// 换号是纯粹空耗还可能误伤账号，具体排除逻辑见各调用点。
//
// 照搬 openAITransportErrorRuleOverride 的取舍：BuiltinWillFailover 恒传 true——传输层
// 失败上内置只有"一律 failover"一种意见，没有"本地写响应"那条链，不必替内置补记账。
// 错误透传规则在这条路径上也问不到：透传规则匹配的是真实上游响应，这里的 502 是合成的，
// 这与"规则引擎优先于透传规则"的口径无关（本函数走的是合成状态码，透传规则匹配条件
// 天然不成立，而不是被谁"让路"）。
func (s *GeminiMessagesCompatService) geminiTransportErrorRuleOverride(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	safeErr string,
) *UpstreamFailoverError {
	body := syntheticTransportRuleBody(safeErr)
	if body == nil {
		return nil
	}
	failoverErr, handled := s.geminiErrorHandlingRuleOverride(ctx, c, geminiErrorHandlingRuleInput{
		Account:             account,
		StatusCode:          geminiTransportRuleSyntheticStatus,
		Header:              http.Header{},
		Body:                body,
		BuiltinWillFailover: true,
		SyntheticStatus:     true,
	})
	if !handled {
		return nil
	}
	return failoverErr
}

func geminiStreamErrorPayload(payload []byte) (int, []byte, bool) {
	if !gjson.GetBytes(payload, "error").Exists() {
		return 0, nil, false
	}
	statusCode := int(gjson.GetBytes(payload, "error.code").Int())
	if statusCode < 400 || statusCode > 599 {
		statusCode = http.StatusBadGateway
	}
	return statusCode, payload, true
}

func (s *GeminiMessagesCompatService) geminiStreamErrorHandlingRuleOverride(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	header http.Header,
	statusCode int,
	body []byte,
	reqModel string,
	message string,
	syntheticStatus bool,
	semanticEventForwarded bool,
) *UpstreamFailoverError {
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "Gemini stream failed"
	}
	if syntheticStatus {
		statusCode = geminiTransportRuleSyntheticStatus
		body = syntheticTransportRuleBody(message)
	}
	failoverErr, handled := s.geminiErrorHandlingRuleOverride(ctx, c, geminiErrorHandlingRuleInput{
		Account:                account,
		StatusCode:             statusCode,
		Header:                 header,
		Body:                   body,
		ReqModel:               reqModel,
		BuiltinWillFailover:    true,
		SyntheticStatus:        syntheticStatus,
		SemanticEventForwarded: semanticEventForwarded,
	})
	if !handled {
		return nil
	}
	s.handleGeminiUpstreamError(ctx, account, statusCode, header, body)
	failoverErr.SafeToFailoverAfterWrite = !semanticEventForwarded
	return failoverErr
}

// logGeminiErrorHandlingRuleDecision 与 OpenAI 侧的 logOpenAIErrorHandlingRuleDecision
// 口径一致。Kind 必须是 "error_handling_rule_" + **生效**动作（不是配置动作）：排查时
// 判断「引擎有没有被绕过」全靠 upstream_errors 里有没有这个前缀，而各平台的 outcome
// 字段必须能直接对比，否则跨平台查询会把配置值和生效值混在一起。
func (s *GeminiMessagesCompatService) logGeminiErrorHandlingRuleDecision(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	in geminiErrorHandlingRuleInput,
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
	// 规则命中会让调用方跳过 writeGeminiMappedError 一类的内置错误处理链，而
	// setOpsUpstreamError 原先只在那条链里调。不补的话，被规则接管的请求在
	// ops_error_logs 里 upstream_status_code 是 NULL。
	//
	// 但**合成**状态码不能写进去：传输层失败根本没有 HTTP 响应，那一列为 NULL 正是
	// 「这是传输层失败」的判定依据。传 0 让 setOpsUpstreamError 只落 message、不动
	// 状态码。
	opsStatusCode := statusCode
	if in.SyntheticStatus {
		opsStatusCode = 0
	}
	setOpsUpstreamError(c, opsStatusCode, sanitizeUpstreamErrorMessage(strings.TrimSpace(upstreamMsg)), upstreamDetail)

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
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
