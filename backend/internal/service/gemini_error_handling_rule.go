package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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
//   - OpenAI：BuiltinWillFailover 与「内置要不要补记账/该不该问透传规则」天然一致；
//   - Gemini：账号记账（handleGeminiUpstreamError）在接线点**之前**就已经跑完（无论
//     内置最终判不判定 failover），但内置判定不 failover 的分支仍会继续走到
//     writeGeminiMappedError/writeGeminiChatCompletionsMappedError，那里会问
//     applyErrorPassthroughRule。
//
// 所以 Gemini 侧接线必须传：
//   - BuiltinWillFailover: shouldFailoverGeminiUpstreamError(statusCode) 的真实结论
//     （不能硬编码 true）——否则会把「给错误透传规则让路」这一件事也一并关掉，让
//     本引擎在 400/404 这类非 failover 状态码上抢走透传规则该处理的错误；
//   - AccountAccounting: nil ——记账已经跑完，执行层的 nil 保护会跳过重复扣分。

// geminiBuiltinOwnsError 判断这条上游错误是否归内置逻辑独占，规则不得抢走。
//
// 逐条核对 Forward / ForwardNative 里在接线点之前就 return 的分支（#228 task-7）：
//   - CheckErrorPolicy 的 switch（ErrorPolicySkipped / ErrorPolicyMatched /
//     ErrorPolicyTempUnscheduled）：自定义错误码与临时不可调度都带账号状态副作用，
//     且已经在接线点之前 return，规则天然碰不到——不需要在这里重复判断。
//   - isGoogleProjectConfigError：确定性的 Google 项目配置类 400，换任何账号都复现，
//     同样已在接线点之前 return。
//
// 这两类已经通过「在接线点之前 return」物理隔离，不会走到
// geminiErrorHandlingRuleOverride，所以 geminiBuiltinOwnsError 本身可以恒为 false——
// 保留函数是为了与 OpenAI 侧的调用形状一致，也为未来出现「接线点之后仍需内置独占」
// 的新分支留一个扩展点。
func geminiBuiltinOwnsError(_ int, _ string, _ []byte) bool {
	return false
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
	// （shouldFailoverGeminiUpstreamError(StatusCode)），不能硬编码 true：该字段在
	// 执行层同时管两件事——(1) 要不要给错误透传规则让路，(2) 要不要替内置补跑账号
	// 记账。Gemini 侧记账（handleGeminiUpstreamError）已经在接线点之前跑完，但非
	// failover 分支仍会走到 writeGeminiMappedError / writeGeminiChatCompletionsMappedError，
	// 那里会问错误透传规则。硬传 true 会把让路逻辑一起关掉。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应）。只用于
	// **匹配**，绝不能写进 ops_error_logs 顶层的 upstream_status_code：那一列为 NULL
	// 正是「这是传输层失败」的判定依据。由 logGeminiErrorHandlingRuleDecision 负责落实。
	SyntheticStatus bool
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

	return executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            settings,
		Account:             account,
		StatusCode:          statusCode,
		Header:              respHeader,
		Body:                respBody,
		ReqModel:            in.ReqModel,
		BuiltinOwns:         geminiBuiltinOwnsError(statusCode, sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody))), respBody),
		BuiltinWillFailover: in.BuiltinWillFailover,
		SyntheticStatus:     in.SyntheticStatus,
		SafeError:           safeGeminiError,
		// 记账已在接线点之前由 handleGeminiUpstreamError 跑完，不再补跑，否则会重复
		// 扣账号健康分。
		AccountAccounting: nil,
		LogDecision: func(decision errorHandlingRuleDecision, effectiveAction string) {
			s.logGeminiErrorHandlingRuleDecision(ctx, c, account, in, decision, effectiveAction)
		},
	})
}

// 本任务（#228 task-7）没有 Gemini 传输层错误的接线点：三条转发链的 HTTP 请求失败
// 都直接返回 error，不经过响应体解析，还没有合成状态码喂给规则引擎的落点。留白由
// 后续任务补，这里先把 logGeminiErrorHandlingRuleDecision 的 SyntheticStatus 落地口径
// 写对（opsStatusCode = 0），避免将来接线时才发现口径错了。
//
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
