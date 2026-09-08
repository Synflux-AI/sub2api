package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 错误处理规则的 Antigravity 执行层。
//
// 决策是平台中立的（error_handling_rule_decider.go），执行是平台特有的：Antigravity
// 的四条转发链（Forward / ForwardGemini / ForwardAsChatCompletions /
// ForwardAsResponses，后两者共用 forwardAntigravityCompat）都挂在同一个
// *AntigravityGatewayService 接收者上，所以这里同 Gemini 一样只需要一个方法，多处
// 复用即可。ForwardUpstream（AccountTypeUpstream 的透传账号）不接线：见该方法上的
// 注释，它是"永远原样透传、从不换号"的单发代理，没有 HTTP 错误分类点可插。
//
// 与 Gemini 侧一个关键的**扩展**（#228 task-8 / 评审修订 §三）：Antigravity 除了
// 三个"标准"接线点（各转发链顶层的错误处理，紧跟在 handleUpstreamError 记账之后），
// 还需要一个**更早**的接线点——antigravityRetryLoop 内部，在它对普通 429/5xx 做
// 内置 3 次指数退避通用重试**之前**。原因：OpenAI 侧的失败处理是"一次失败就问"，
// 而 Antigravity 的内置重试循环会先烧掉最多 3 次同账号请求才把控制权交回三条转发链
// 顶层。如果只接三个标准点，管理台配的"立即换号"/"原样透传"规则要等内置重试全部
// 耗尽才生效——语义上不等价于 OpenAI，还会对一个确定会被规则接管的错误发出多余的
// 上游请求。见 antigravityRetryLoopParams.ruleOverride 与 antigravity_gateway_retry.go
// 里的两处调用点。
//
// 这个早接线点**不**进入 OAuth 专用的纠错分支（handleSmartRetry 的智能重试/AI
// Credits 超量重试/MODEL_CAPACITY_EXHAUSTED 固定重试，以及各转发链里请求体会被
// 改写的 signature/thinking-budget 纠错重试）：这些纠错路径依赖内置对上游返回的
// 结构化字段（RetryInfo/ErrorInfo）做出的专属决策，不是"规则 vs 内置"的竞争关系，
// 保持内置独占。ruleOverride 钩子只挂在 antigravityRetryLoop 主循环里"其他可重试
// 429/503/500/502/504/529"的两个通用分支上，且只在 attempt==1（首次失败）时问一次。
//
// 与 OpenAI/Gemini 侧一处共同的语义约束：
//   - Antigravity：三个标准接线点上，账号记账（handleUpstreamError）都在接线点**之前**
//     就已经跑完（无论内置最终判不判定 failover），非 failover 分支仍会继续走到各链
//     自己的 writeMapped*Error，那里会问 applyErrorPassthroughRule。早接线点则相反：
//     账号记账**尚未**跑，但 BuiltinWillFailover 在那个位置恒为 true（见下方
//     antigravityEarlyRuleOverrideHook 的文档），执行层的补记账保护天然不会触发，
//     AccountAccounting: nil 在两类接线点上都是安全且正确的选择。
//
// 所以 Antigravity 侧接线必须传：
//   - BuiltinWillFailover: s.shouldFailoverUpstreamError(statusCode) 的真实结论
//     （不能硬编码 true）——它只决定「规则接管时要不要替内置补跑账号记账」
//     （AccountAccounting），跟错误透传规则的优先级无关：2026-09-08 起规则引擎
//     全链优先于透传规则，不再有"让路"（见 error_handling_rule_executor.go）。
//     早接线点上这个值可证明恒为 true（shouldFailoverUpstreamError 覆盖
//     的状态码集合正好等于能走到早接线点的状态码集合），但仍然按真实分类传，不写
//     死常量，避免未来任一侧的状态码集合变化后悄悄产生分歧。
//   - AccountAccounting: nil ——三个标准接线点上记账已经跑完；早接线点上记账还没跑，
//     但 BuiltinWillFailover 恒为 true 使执行层的补记账逻辑天然不触发，nil 同样安全。

// antigravityBuiltinOwnsError 判断这条上游错误是否归内置逻辑独占，规则不得抢走。
//
// 覆盖两类错误，逐条核对（#228 task-8，第二类是 fix round 1 补的）：
//
// 【类 1】400 + Google 项目配置类消息（isGoogleProjectConfigError）。四条转发链在这一点
// 上的状况并不对称：
//   - Forward（claude 协议链）：isPromptTooLongError 与 isGoogleProjectConfigError 的
//     400 特判，都已经在接线点之前物理 return，规则引擎压根碰不到这两类错误。这里
//     对这条链而言是双重保险——就算判了 true，也不会改变已经在更早处 return 的结果。
//     该链的 msg400 构造是 ToLower(TrimSpace(extractAntigravityErrorMessage(respBody)))，
//     不经过 sanitizeUpstreamErrorMessage（antigravity_gateway_claude.go 里的 msg 变量）。
//   - ForwardGemini（gemini 协议链）：isGoogleProjectConfigError 的 400 特判同样在
//     接线点之前物理 return（模型不存在 fallback / signature 纠错重试成功后会
//     goto handleSuccess 绕开整个错误分支，不影响这里的判断）。双重保险，同上。
//     但**这条链的 msg400 构造与 Forward 不同**：ForwardGemini 先把 upstreamMsg 整个
//     经过 sanitizeUpstreamErrorMessage，再 ToLower 传给 isGoogleProjectConfigError
//     （antigravity_gateway_gemini.go：`upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)`
//     在先，`isGoogleProjectConfigError(strings.ToLower(upstreamMsg))` 在后）——两条链的
//     口径不是逐字一致的两份实现，只是碰巧都在接线点之前物理 return，这里的判断本身
//     对这条链而言仍然只是双重保险，不受影响。
//   - forwardAntigravityCompat（chat completions / responses 协议链，共用
//     handleAntigravityCompatHTTPError）：**没有任何早退分支**，也没有
//     isGoogleProjectConfigError 特判。这里如果恒为 false，一条形如「400 → failover」
//     的管理台规则就能抢下一个确定性的 Google 项目配置错误——这种错误换任何账号都
//     复现，被规则接管意味着整个账号池会被无谓地打一遍换号，是纯粹的行为退化。所以
//     这个判断在这条链上是**承重的**，不能省，也不能只留个恒 false 的占位。
//   - antigravityRetryLoop 内部的早接线点（普通 429/5xx 首次失败）：能走到那两个
//     调用点的状态码集合是 {429,503}（智能重试分支的 fallthrough）与
//     {500,502,504,529}（shouldRetryAntigravityError），Google 项目配置类错误固定是
//     400，物理上不会出现在这两个分支——这里的判断在早接线点上是恒定的双重保险，
//     不承重，但同一个判断函数必须覆盖所有调用点，不能为早接线点单独做一份不同的
//     判定，否则两处判定可能漂移。
//
// 检测口径：ToLower + TrimSpace 作用于 extractAntigravityErrorMessage 的结果（不经过
// sanitizeUpstreamErrorMessage）——这是 antigravityErrorHandlingRuleOverride 里
// lowerMsg 的构造口径（extractAntigravityErrorMessage 是本文件族的原生消息提取函数，
// 而不是 Gemini 侧 gjson 版本的 extractUpstreamErrorMessage），调用方已按这个口径把
// lowerMsg 准备好再传进来；跟 Forward/ForwardGemini 各自 msg400 是否逐字一致无关——
// 那两条链上这个判断都是双重保险，判不判 true 都不改变已经在更早处 return 的结果。
//
// 【类 2】401 凭据被拒。compat 链的 handleAntigravityCompatHTTPError 在
// shouldFailoverUpstreamError(401)==true 之后单独 if 出一支，调用
// antigravityCredentialRejectedError(resp, body) 产出带类型字段的 UpstreamFailoverError：
// Stage=GatewayFailureStageAccountAuth、Scope=GatewayFailureScopeAccount、
// Reason=AntigravityCredentialRejectedReason、ClientStatusCode=502、
// ClientMessage=AntigravityCredentialRejectedClientMessage。这些字段是下游归因与文案的
// 唯一来源：ops_service.go 靠 Stage==AccountAuth 才会把这条记录归因为凭据失败而不是
// 普通推理失败；账号池耗尽时 credentialFailoverClientResponse 靠 Reason 才能给出
// Antigravity 自己的凭据失败文案，读不到 Reason 就会落到 GrokCredentialUnavailableClientMessage
// ——另一个平台的兜底文案，把 Antigravity 的失败说成 Grok 的话。规则版错误是另起一个
// UpstreamFailoverError，带不出这些字段：一条形如「401 → failover」的管理台规则会把
// 这条归因和专属文案一起清零。与 openAIBuiltinOwnsError（openai_error_handling_rule.go）
// 对 access-state/凭据类错误的先例是同一类判断：那里的注释点名"会把 413 的专用文案、
// 凭据失败的归因、容量削峰的「不扣账号健康分」一起清零"，Antigravity 的 401 是同一种
// 「带类型字段的错误不能被裸的规则版错误替换」的形状。
//
// 检测口径：只按状态码，不看响应体——antigravityCredentialRejectedError 本身的分支
// 条件也只是 resp.StatusCode == http.StatusUnauthorized，不检查任何 body 字段，两处
// 判定必须保持同一个口径，多加条件只会让两处判定漂移。
//
// 这条判断故意不按调用链限定范围，对 Forward/ForwardGemini/早接线点同样生效，尽管
// 目前只有 compat 链会真正构造这些 typed 字段：claude.go/gemini.go 里搜不到
// StatusUnauthorized，没有任何 401 专属分支；早接线点的两处调用点只处理
// {429,503} 与 {500,502,504,529}，401 在 antigravityRetryLoop 里走的是"其他 4xx
// 错误直接返回"，根本不会问到 ruleOverride，所以早接线点在这条判断上完全不受影响。
// 在 claude/gemini 这两条链上把这条判断落成 true，代价是这两条链上不能再单独配一条
// 「401 → retry/passthrough」的管理台规则——之前是可以的，现在也归内置独占了。这是
// 刻意的取舍，不是疏忽：401 是凭据问题，同账号 retry 修不好，passthrough 把裸 401 还
// 给客户端也比不上内置的换号语义；让同一个共享判断函数在不同调用方之间表现不同，正是
// 两处判定漂移的起点，统一按状态码判断、不区分调用链，换来的是判定函数本身不需要关心
// 调用方是谁。
//
// 保持窄：只处理这两种确定性错误，不要顺手加别的条件——新分支需要新的证据。
func antigravityBuiltinOwnsError(statusCode int, upstreamMsg string, _ []byte) bool {
	if statusCode == http.StatusUnauthorized {
		return true
	}
	return statusCode == http.StatusBadRequest && isGoogleProjectConfigError(upstreamMsg)
}

// safeAntigravityError 从上游错误体里取出可安全返回给客户端的类型与消息。
//
// Antigravity 的错误体在到达任何一个接线点时都还是 Google 原生形状
// {"error":{"code":N,"message":"...","status":"..."}}（Claude/compat 协议转换只发生在
// 成功路径，错误路径上响应体没有经过任何协议转换），errType 取 status 字段（如
// RESOURCE_EXHAUSTED），缺失时退回 "upstream_error"。结构与 safeGeminiError 一致，
// 因为两者面对的是同一个 Google 后端的同一种错误形状。
func safeAntigravityError(body []byte) (string, string) {
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
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractAntigravityErrorMessage(body)))
	if message == "" {
		message = "Upstream request failed"
	}
	return "upstream_error", message
}

// antigravityErrorHandlingRulesActive 是热路径早退出：没配规则、没勾 antigravity、
// 账号不是 Antigravity 平台时，一次配置读取以外什么都不做。
//
// 不卡账号类型：不区分 OAuth / APIKey / Upstream。apikey 类型账号走
// GatewayService.Forward（由另一个任务接线），upstream 类型账号走 ForwardUpstream
// （本任务确认无接线点，见该方法上的注释）；本文件覆盖的四条转发链全部只服务于
// AntigravityGatewayService 自己的 OAuth 账号方法集，不需要在这里用账号类型二次
// 设限——真正的风险点（如果有）应该进 antigravityBuiltinOwnsError。
func (s *AntigravityGatewayService) antigravityErrorHandlingRulesActive(ctx context.Context, account *Account) (ErrorHandlingRuleSettings, bool) {
	if s == nil || s.settingService == nil || account == nil || account.Platform != PlatformAntigravity {
		return ErrorHandlingRuleSettings{}, false
	}
	settings := s.settingService.GetErrorHandlingRuleSettingsCached(ctx)
	if !settings.Enabled || !HasEnabledErrorHandlingRuleForPlatform(settings.Rules, account.Platform) {
		return ErrorHandlingRuleSettings{}, false
	}
	return settings, true
}

// antigravityErrorHandlingRuleInput 是 Antigravity 执行层的一次提问。用结构体而不是
// 位置参数：里面有多个 int/bool，位置传参很容易在新增接入点时错位。
type antigravityErrorHandlingRuleInput struct {
	Account    *Account
	StatusCode int
	Header     http.Header
	Body       []byte
	ReqModel   string

	// BuiltinWillFailover 必须传真实的内置分类结论
	// （s.shouldFailoverUpstreamError(StatusCode)），不能硬编码 true：执行层只用它
	// 决定「规则接管时要不要替内置补跑账号记账」（AccountAccounting）。三个标准
	// 接线点上记账（handleUpstreamError）已经在接线点之前跑完；早接线点上记账
	// 还没跑，但这个值在那个位置可证明恒为 true，执行层的补记账逻辑天然不触发。
	// 与错误透传规则的优先级无关：2026-09-08 起规则引擎全链优先，不再有"让路"。
	BuiltinWillFailover bool

	// SyntheticStatus 表示 StatusCode 是合成的（传输层错误没有 HTTP 响应）。只用于
	// **匹配**，绝不能写进 ops_error_logs 顶层的 upstream_status_code：那一列为 NULL
	// 正是「这是传输层失败」的判定依据。由 logAntigravityErrorHandlingRuleDecision
	// 负责落实。四条转发链在拿到真实上游 HTTP 响应之后才会问规则引擎的三个标准接线点
	// 与 antigravityRetryLoop 里的早接线点上恒为 false；antigravityTransportErrorRuleOverride
	// （#228 task-10，antigravityRetryLoop 内部 Do() 失败、重试与 URL fallback 都耗尽
	// 之后调用）上恒为 true。
	SyntheticStatus bool
}

// antigravityErrorHandlingRuleOverride 问一次规则引擎，命中就返回规则版的 failover
// 错误。
//
// handled == false 时调用方必须原样走内置路径——未命中的请求一行行为都不能变。
// Forward / ForwardGemini / forwardAntigravityCompat（含 ForwardAsChatCompletions /
// ForwardAsResponses）以及 antigravityRetryLoop 内部的早接线点共用这一个方法。
func (s *AntigravityGatewayService) antigravityErrorHandlingRuleOverride(
	ctx context.Context,
	c *gin.Context,
	in antigravityErrorHandlingRuleInput,
) (*UpstreamFailoverError, bool) {
	account := in.Account
	statusCode := in.StatusCode
	respBody := in.Body
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}

	settings, active := s.antigravityErrorHandlingRulesActive(ctx, account)
	if !active {
		return nil, false
	}

	// ToLower + TrimSpace 作用于 extractAntigravityErrorMessage 的结果，不经过
	// sanitizeUpstreamErrorMessage。这与 Forward（claude 协议链）的 msg400 构造逐字
	// 一致，但与 ForwardGemini 不同——ForwardGemini 会先经过 sanitizeUpstreamErrorMessage
	// 再 ToLower（见 antigravity_gateway_gemini.go）。两条链在这一点上不是同一份实现，
	// 但都在这里的判断生效之前已经物理 return 过，所以口径差异对 Forward/ForwardGemini
	// 无影响；对没有早退分支的 compat 链，这里统一用的是本行的口径。
	// antigravityBuiltinOwnsError 靠这个口径识别确定性的 Google 项目配置类 400
	// （见该函数的文档注释）。
	lowerMsg := strings.ToLower(strings.TrimSpace(extractAntigravityErrorMessage(respBody)))

	return executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            settings,
		Account:             account,
		StatusCode:          statusCode,
		Header:              respHeader,
		Body:                respBody,
		BuiltinOwns:         antigravityBuiltinOwnsError(statusCode, lowerMsg, respBody),
		BuiltinWillFailover: in.BuiltinWillFailover,
		SyntheticStatus:     in.SyntheticStatus,
		SafeError:           safeAntigravityError,
		// 记账已在三个标准接线点之前由 handleUpstreamError 跑完；早接线点上记账尚未
		// 跑，但 BuiltinWillFailover 在那个位置恒为 true，执行层的补记账保护
		// （!BuiltinWillFailover && AccountAccounting != nil）天然不会触发。两类接线
		// 点上 nil 都是安全且正确的选择，不需要区分。
		AccountAccounting: nil,
		LogDecision: func(decision errorHandlingRuleDecision, effectiveAction string) {
			s.logAntigravityErrorHandlingRuleDecision(ctx, c, account, in, decision, effectiveAction)
		},
	})
}

// antigravityTransportRuleSyntheticStatus 是传输层错误喂给规则引擎时用的合成状态码。
//
// 传输层失败没有 HTTP 响应，引擎又只看状态码+响应体，所以这里合成一个。与 OpenAI /
// Gemini 侧同一先例（openAITransportRuleSyntheticStatus / geminiTransportRuleSyntheticStatus）。
const antigravityTransportRuleSyntheticStatus = http.StatusBadGateway

// antigravityTransportErrorRuleOverride 把没有完整可用 HTTP 响应的传输层错误合成成
// 502 + 统一形状的错误体（syntheticTransportRuleBody）后问规则。命中返回规则版的
// failover 错误，未命中返回 nil。
//
// 唯一调用点在 antigravityRetryLoop 内部（antigravity_gateway_retry.go）：Do() 失败、
// 同账号重试与 URL fallback 全部耗尽之后（#228 task-10）。这是 ForwardGemini
// （antigravity_gateway_gemini.go）与 forwardAntigravityCompat/
// handleAntigravityCompatTransportError（antigravity_gateway_compat.go）共同依赖的
// 唯一"连接失败、重试用尽"出口——两条转发链都已经在这个 err 上先判过
// `err.(*UpstreamFailoverError)`，命中就直接把它当 failover 错误交给 Handler，所以
// 只需要在 antigravityRetryLoop 这一处接线，两条转发链自动生效，不需要分别在
// ForwardGemini / handleAntigravityCompatTransportError 里各插一次。
//
// 调用方必须先排除客户端断连（p.ctx.Err() != nil）：那种情况下 upstream 请求已经
// 打出去，没人会读响应，规则换号是纯粹空耗还可能误伤账号——具体排除逻辑见
// antigravityRetryLoop 里的调用点。
//
// 照搬 openAITransportErrorRuleOverride 的取舍：BuiltinWillFailover 恒传 true——传输层
// 失败上内置只有"一律 failover"一种意见，没有"本地写响应"那条链，不必替内置补记账。
// 错误透传规则在这条路径上也问不到：透传规则匹配的是真实上游响应，这里的 502 是合成的，
// 这与"规则引擎优先于透传规则"的口径无关（本函数走的是合成状态码，透传规则匹配条件
// 天然不成立，而不是被谁"让路"）。
func (s *AntigravityGatewayService) antigravityTransportErrorRuleOverride(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	safeErr string,
) *UpstreamFailoverError {
	body := syntheticTransportRuleBody(safeErr)
	if body == nil {
		return nil
	}
	failoverErr, handled := s.antigravityErrorHandlingRuleOverride(ctx, c, antigravityErrorHandlingRuleInput{
		Account:             account,
		StatusCode:          antigravityTransportRuleSyntheticStatus,
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

// antigravityRuleOverrideHook 是喂给 antigravityRetryLoop 的早接线钩子类型。
// 只接收已经拿到的上游 HTTP 响应的三个字段——account/ctx/c/请求模型等由构造方
// （antigravityEarlyRuleOverrideHook）捕获进闭包，不需要每次调用都重新传递。
type antigravityRuleOverrideHook func(statusCode int, header http.Header, body []byte) (*UpstreamFailoverError, bool)

// antigravityEarlyRuleOverrideHook 构造 antigravityRetryLoopParams.ruleOverride。
//
// 只用于 antigravityRetryLoop 内部"纠错特例未命中之后的普通 429/5xx"分支（见
// antigravity_gateway_retry.go 里的两处调用点），且调用方只在 attempt==1（首次失败）
// 时问一次——命中就直接把内置的 3 次通用重试短路掉，未命中就照旧走内置重试。
//
// BuiltinWillFailover 用 s.shouldFailoverUpstreamError(statusCode) 现算，不硬编码
// true：虽然能走到这两个调用点的状态码集合（{429,503} 与 {500,502,504,529}）正好是
// shouldFailoverUpstreamError 判 true 的状态码集合的子集，此刻恒为 true，但仍按真实
// 分类现算，避免两个状态码集合未来独立演化后悄悄产生分歧（trap，见 #228 task-8
// 交接记录）。
//
// SyntheticStatus 恒为 false：这个钩子只在拿到真实上游 HTTP 响应之后才会被调用，从不
// 处理传输层错误。
func (s *AntigravityGatewayService) antigravityEarlyRuleOverrideHook(ctx context.Context, c *gin.Context, account *Account, reqModel string) antigravityRuleOverrideHook {
	return func(statusCode int, header http.Header, body []byte) (*UpstreamFailoverError, bool) {
		return s.antigravityErrorHandlingRuleOverride(ctx, c, antigravityErrorHandlingRuleInput{
			Account:             account,
			StatusCode:          statusCode,
			Header:              header,
			Body:                body,
			ReqModel:            reqModel,
			BuiltinWillFailover: s.shouldFailoverUpstreamError(statusCode),
			SyntheticStatus:     false,
		})
	}
}

// logAntigravityErrorHandlingRuleDecision 与 Gemini 侧的
// logGeminiErrorHandlingRuleDecision 口径一致。Kind 必须是 "error_handling_rule_" +
// **生效**动作（不是配置动作）：排查时判断「引擎有没有被绕过」全靠 upstream_errors
// 里有没有这个前缀，而各平台的 outcome 字段必须能直接对比，否则跨平台查询会把配置值
// 和生效值混在一起。
func (s *AntigravityGatewayService) logAntigravityErrorHandlingRuleDecision(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	in antigravityErrorHandlingRuleInput,
	decision errorHandlingRuleDecision,
	effectiveAction string,
) {
	statusCode := in.StatusCode
	respBody := in.Body
	respHeader := in.Header
	if respHeader == nil {
		respHeader = http.Header{}
	}
	upstreamMsg := extractAntigravityErrorMessage(respBody)
	upstreamDetail := s.getUpstreamErrorDetail(respBody)

	// ops_error_logs 的顶层列（upstream_status_code / message）必须在这里补一次：
	// 规则命中会让调用方跳过各链自己的错误处理路径（writeMappedClaudeError /
	// c.Data(...) / writeMappedAntigravityCompatError），那几条路径原先才是
	// setOpsUpstreamError 唯一的落点。不补的话，被规则接管的请求在 ops_error_logs 里
	// upstream_status_code 是 NULL。
	//
	// 但**合成**状态码不能写进去：传输层失败根本没有 HTTP 响应，那一列为 NULL 正是
	// 「这是传输层失败」的判定依据。传 0 让 setOpsUpstreamError 只落 message、不动
	// 状态码。三个标准接线点与早接线点上 SyntheticStatus 恒为 false；
	// antigravityTransportErrorRuleOverride（#228 task-10）上恒为 true，这里的分支
	// 与 Gemini/OpenAI 保持同一口径。
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
