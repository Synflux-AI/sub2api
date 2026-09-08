package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 错误处理规则的 Grok media（图片/视频生成）执行层。
//
// 决策是平台中立的（error_handling_rule_decider.go），执行是平台特有的：
// handleGrokMediaErrorResponse 是 ForwardGrokMedia（生成路径）与
// forwardGrokMediaVideoContent（video_content 查询，内部还会先查一次 video_status）
// 共用的一个错误处理函数，三个调用点全部挂在同一个 *OpenAIGatewayService 接收者上。
//
// ⚠️ 与 Gemini/Antigravity/OpenAI 都不同的关键差异（#228 task-9）：这个共用函数里
// 有两类调用方**不能**接规则引擎。video_status / video_content 查询绑定到创建任务时
// 选中的原账号（owner binding，由 internal/handler/grok_media.go 里
// ResolveGrokMediaVideoRequestAccount 强制校验，选号阶段就已经把候选池收窄成那一个
// 账号），语义上不允许换号。如果在 handleGrokMediaErrorResponse 内部不分调用方一律
// 接规则引擎，一条配了 failover 的管理台规则会让 ops_error_logs 记下
// "error_handling_rule_failover"、看起来规则已经接管换号，而 owner binding 实际上
// 仍然会阻止真正换号（handler 层对 endpoint.IsVideoLookupRequest() 有独立的立即终止
// 分支，见 grok_media.go handler 里 `if endpoint.IsVideoLookupRequest() {
// h.handleFailoverExhausted(...); return }`）——规则看起来生效了，实际什么都没做，
// 这正是 #228 评审修订 §六要消灭的"假生效"这一类问题。
//
// 收口方式是**允许清单**而不是排除清单：接线只在 endpoint.IsGenerationRequest()==true
// 时问规则引擎（见 grok_media.go 里 handleGrokMediaErrorResponse 的接线点注释），
// 新增的 GrokMediaEndpoint 常量默认落在清单外，除非显式加进 IsGenerationRequest()——
// 这样以后新增一个查询类端点忘了排除也不会悄悄接上规则引擎。
//
// 与 Gemini/Antigravity 侧一处共同的语义约束：
//   - Grok media：账号记账（handleGrokAccountUpstreamError）在接线点**之前**就已经
//     跑完（handleGrokMediaErrorResponse 函数体第一行就调用），无论内置最终判不判定
//     failover。
//
// 所以 Grok media 侧接线必须传：
//   - BuiltinWillFailover: s.shouldFailoverGrokUpstreamError(StatusCode, Body) 的真实
//     结论（不能硬编码 true）——它只决定规则接管时要不要替内置补跑账号记账
//     （AccountAccounting），跟错误透传规则无关（见下）；
//   - AccountAccounting: nil ——记账已经跑完，执行层的 nil 保护会跳过重复扣分。
//
// 错误处理规则与错误透传规则的次序：2026-09-08 起规则引擎全链优先——
// handleGrokMediaErrorResponse 里先问本函数（endpoint.IsGenerationRequest() 为真时），
// 命中就直接返回；未命中（含未开启/未配置匹配规则/video_status·video_content 这类
// 不允许接线的查询端点）才轮到 applyErrorPassthroughRule。历史：#228 早期这里反过来
// （先查透传规则、命中就物理 return，规则引擎连被问到的机会都没有），已按项目所有者
// 决定纠正。

// grokMediaBuiltinOwnsError 判断这条上游错误是否归内置逻辑独占，规则不得抢走。
//
// 只覆盖一类：isGrokContentPolicyRejection（内容策略拒绝，通常是 403 + 特定错误码/
// 消息）。handleGrokMediaErrorResponse 在接线点**之前**就已经对这类错误物理 return
// （见该函数体，先问 isGrokContentPolicyRejection 再问规则引擎/透传规则），
// 规则引擎压根碰不到——这里判 true 只是双重保险，不改变已经在更早处 return 的结果。
//
// 审计过 grok_media.go 与 Grok failover 路径上所有会在 UpstreamFailoverError 上
// 计算"带类型字段"（Reason/Scope/Stage/ClientStatusCode/ClientMessage）的错误类：
//   - 凭据失败（newGrokCredentialFailover，Stage=AccountAuth/ClientStatusCode=503/
//     ClientMessage=GrokCredentialUnavailableClientMessage）：发生在 getRequestCredential，
//     ForwardGrokMedia 里在任何上游 HTTP 请求之前就已经 return，物理上不可能进入
//     handleGrokMediaErrorResponse。
//   - 流式聊天空闲超时（grokStreamIdleFailoverError，RequestScopedTransient=true/
//     SameAccountRetryMax=1）：是完全不同的代码路径（流式聊天补全的空闲检测），跟
//     grok media 的图片/视频生成、HTTP 错误响应处理没有任何调用关系。
//   - 本函数唯二能触达的失败分支（handleGrokMediaErrorResponse 里 kind=="failover"
//     那一段）只设置 RetryableOnSameAccount / RequestScopedTransient /
//     SameAccountRetryMax 三个字段，不设置 Reason/Scope/Stage/ClientStatusCode/
//     ClientMessage——即使规则接管，也没有这几个字段会被规则版错误顶掉。
//     RequestScopedTransient 唯一可观测的下游消费点是
//     internal/handler/failover_loop.go 的 sameAccountRetryDelayFor（决定同账号重试
//     退避是否走指数曲线），Grok media 的 handler（internal/handler/grok_media.go）
//     没有调用 TempUnscheduleRetryableError，也不会调 ShouldReportAccountScheduleFailure
//     以外的字段消费点，所以规则接管后失去这个字段只影响重试节奏，不影响归因/文案/
//     健康分——不是 openAIBuiltinOwnsError 文档里点名的那一类"typed 字段被顶掉造成
//     误判"的伤害。SameAccountRetryMax 同理：effectiveSameAccountRetryLimit 已经把
//     RuleRetryLimit（规则显式配的）设计成优先于 SameAccountRetryMax（错误自带的硬
//     上限）——一条管理台规则显式配了重试预算，本来就该覆盖 Grok 容量类错误的默认
//     上限，这是既有设计，不是本任务需要防的漂移。
//
// 保持窄：只处理这一种确定性错误，不要顺手加别的条件——新分支需要新的证据。
func grokMediaBuiltinOwnsError(statusCode int, upstreamMsg string, respBody []byte) bool {
	_ = upstreamMsg
	return isGrokContentPolicyRejection(statusCode, respBody)
}

// safeGrokMediaError 从上游错误体里取出可安全返回给客户端的类型与消息。
//
// Grok（xAI）的错误体形状不固定，常见的是 {"error":{"code":"...","message":"..."}}，
// 也可能是裸字符串或纯文本。先用 parseGrokUpstreamErrorJSON（grok_upstream_failure.go，
// Grok 失败分类复用的同一个解析器）取 code/message，取不到消息时退回通用的
// extractUpstreamErrorMessage。
func safeGrokMediaError(body []byte) (string, string) {
	code, message := parseGrokUpstreamErrorJSON(strings.TrimSpace(string(body)))
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	}
	if message == "" {
		message = "Upstream request failed"
	}
	errType := strings.TrimSpace(code)
	if errType == "" {
		errType = "upstream_error"
	}
	return errType, message
}

// grokMediaErrorHandlingRulesActive 是热路径早退出：没配规则、没勾 grok、账号不是
// Grok 平台时，一次配置读取以外什么都不做。
//
// 不卡账号类型：不区分 OAuth / APIKey。真正的风险点（如果有）应该进
// grokMediaBuiltinOwnsError，不该用账号类型二次设限。
func (s *OpenAIGatewayService) grokMediaErrorHandlingRulesActive(ctx context.Context, account *Account) (ErrorHandlingRuleSettings, bool) {
	if s == nil || s.settingService == nil || account == nil || account.Platform != PlatformGrok {
		return ErrorHandlingRuleSettings{}, false
	}
	settings := s.settingService.GetErrorHandlingRuleSettingsCached(ctx)
	if !settings.Enabled || !HasEnabledErrorHandlingRuleForPlatform(settings.Rules, account.Platform) {
		return ErrorHandlingRuleSettings{}, false
	}
	return settings, true
}

// grokMediaErrorHandlingRuleInput 是 Grok media 执行层的一次提问。用结构体而不是
// 位置参数：里面有多个 int/bool，位置传参很容易在新增接入点时错位。
type grokMediaErrorHandlingRuleInput struct {
	Account    *Account
	StatusCode int
	Header     http.Header
	Body       []byte
	ReqModel   string

	// BuiltinWillFailover 必须传真实的内置分类结论
	// （s.shouldFailoverGrokUpstreamError(StatusCode, Body)），不能硬编码 true：执行层
	// 只用它决定「规则接管时要不要替内置补跑账号记账」（AccountAccounting）——
	// handleGrokMediaErrorResponse 里账号记账（handleGrokAccountUpstreamError）已经在
	// 接线点之前跑完，硬传 true 只影响这一项，与错误透传规则的优先级无关（见
	// grok_media_error_handling_rule.go 头部注释：2026-09-08 起规则引擎全链优先于
	// 透传规则，不再有"让路"）。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应）。只用于
	// **匹配**，绝不能写进 ops_error_logs 顶层的 upstream_status_code：那一列为 NULL
	// 正是「这是传输层失败」的判定依据。由 logGrokMediaErrorHandlingRuleDecision
	// 负责落实。本任务（#228 task-9）的接线点在拿到真实上游 HTTP 响应之后才会问规则
	// 引擎，恒为 false；Grok media 传输层错误（请求发送失败，走
	// handleOpenAIUpstreamTransportError）的接线留给后续任务。
	SyntheticStatus bool
}

// grokMediaErrorHandlingRuleOverride 问一次规则引擎，命中就返回规则版的 failover
// 错误。
//
// handled == false 时调用方必须原样走内置路径——未命中的请求一行行为都不能变。
// 只应在 endpoint.IsGenerationRequest()==true 时调用（见 grok_media.go 的接线点
// 注释）：video_status / video_content 查询按 #228 §六不得接线。
func (s *OpenAIGatewayService) grokMediaErrorHandlingRuleOverride(
	ctx context.Context,
	c *gin.Context,
	in grokMediaErrorHandlingRuleInput,
) (*UpstreamFailoverError, bool) {
	account := in.Account
	statusCode := in.StatusCode
	respBody := in.Body
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}

	settings, active := s.grokMediaErrorHandlingRulesActive(ctx, account)
	if !active {
		return nil, false
	}

	// 与 handleGrokMediaErrorResponse 里 upstreamMsg 的构造方式一致：
	// sanitizeUpstreamErrorMessage(TrimSpace(extractUpstreamErrorMessage(body)))。
	// grokMediaBuiltinOwnsError 本身不消费这个参数（isGrokContentPolicyRejection 只看
	// 状态码与原始 body），但保留同一口径便于后续扩展。
	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))

	return executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            settings,
		Account:             account,
		StatusCode:          statusCode,
		Header:              respHeader,
		Body:                respBody,
		ReqModel:            in.ReqModel,
		BuiltinOwns:         grokMediaBuiltinOwnsError(statusCode, upstreamMsg, respBody),
		BuiltinWillFailover: in.BuiltinWillFailover,
		SyntheticStatus:     in.SyntheticStatus,
		SafeError:           safeGrokMediaError,
		// 记账已在接线点之前由 handleGrokAccountUpstreamError 跑完，不再补跑，否则会
		// 重复扣账号健康分/额度状态。
		AccountAccounting: nil,
		LogDecision: func(decision errorHandlingRuleDecision, effectiveAction string) {
			s.logGrokMediaErrorHandlingRuleDecision(ctx, c, account, in, decision, effectiveAction)
		},
	})
}

// logGrokMediaErrorHandlingRuleDecision 与 Gemini/Antigravity 侧的
// logGeminiErrorHandlingRuleDecision/logAntigravityErrorHandlingRuleDecision 口径
// 一致。Kind 必须是 "error_handling_rule_" + **生效**动作（不是配置动作）：排查时
// 判断「引擎有没有被绕过」全靠 upstream_errors 里有没有这个前缀，而各平台的 outcome
// 字段必须能直接对比，否则跨平台查询会把配置值和生效值混在一起。
func (s *OpenAIGatewayService) logGrokMediaErrorHandlingRuleDecision(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	in grokMediaErrorHandlingRuleInput,
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

	// 与 handleGrokMediaErrorResponse 自身的 upstreamDetail 构造方式（maxBytes<=0 时
	// 兜底 2048）保持一致，而不是照抄 Gemini 侧的实现——这是 grok_media.go 这一片
	// 代码既有的口径。
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(respBody), maxBytes)
	}

	// ops_error_logs 的顶层列（upstream_status_code / message）必须在这里补一次：
	// 规则命中会让调用方跳过 handleGrokMediaErrorResponse 剩下的内置错误处理路径
	// （applyErrorPassthroughRule 之后的 ShouldHandleErrorCode / failover / 兜底写
	// 响应三段），那几段原先才是 setOpsUpstreamError 唯一的落点。不补的话，被规则
	// 接管的请求在 ops_error_logs 里 upstream_status_code 是 NULL。
	//
	// 但**合成**状态码不能写进去：传输层失败根本没有 HTTP 响应，那一列为 NULL 正是
	// 「这是传输层失败」的判定依据。传 0 让 setOpsUpstreamError 只落 message、不动
	// 状态码。本任务的接线点 SyntheticStatus 恒为 false，这里的分支是为了与
	// Gemini/Antigravity 保持同一口径、并给后续任务接传输层错误留好落点。
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
		// 合成状态码只用于匹配，单独记一条，免得排查时把它当成真实上游状态码。
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
