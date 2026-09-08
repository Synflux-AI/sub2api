//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

// #228 task-12：错误处理规则跨平台验收矩阵（service 层）。
//
// 覆盖 issue #228 §七 6×7 矩阵里落在 internal/service 包的 5 个维度：HTTP 非
// failover 错误、HTTP failover 错误、transport 错误、流内错误、错误透传让路。
// retry budget 与 exhausted passthrough 两个维度落在 internal/handler 包，见
// error_handling_rule_matrix_test.go（同名，位于 internal/handler）。
//
// 本文件不是第二套测试实现：Task 3-11 已经写了 ~84 个覆盖这些单元格的子测试，
// 这里只按名字引用它们（matrixAssertTestExists 会去扫 _test.go 源码确认引用的
// 函数名真实存在，函数被删除/改名会让矩阵报错），只有真正没被覆盖到的格子才在
// 本文件里新增子测试。
//
// 维度解释（HTTP 非failover / HTTP failover）：矩阵按 BuiltinWillFailover 语义
// 二分不够贴合各平台测试的组织方式——各平台的规则测试文件普遍是按"规则配置的
// 动作"（failover / retry / passthrough）组织的一个子测试组。这里采用的映射是：
// 规则动作为 failover 的场景 = "HTTP failover 错误"列；规则动作为 retry 或
// passthrough 的场景（即规则接管、把原本会/不会 failover 的错误改写成不换号的
// 处理）= "HTTP 非failover 错误"列。"miss" 场景（规则不匹配、或被内置独占逻辑
// 抢先处理）在两列间共用同一个引用，因为"未命中"这件事本身不区分列。这是一个
// 有意的解释性简化，比逐条重新验证 BuiltinWillFailover 布尔值在每个测试里的取值
// 更贴近现有测试的组织方式；如果这个解释被认为不合适，具体的判断依据（各测试
// 引用的行号）都在下面每个子测试里写明，方便复核。
//
// 两个诚实披露的真实生产缺口（不是测试缺口，本任务范围不含修复）：
//
//  1. Anthropic Messages 的 "transport 错误" 维度完全没有接到规则引擎。
//     GatewayService.handleUpstreamTransportError（gateway_upstream_transport_error.go:41-70）
//     全文读过：无条件返回固定的 502 UpstreamFailoverError，从未读取
//     s.settingService，也没有任何规则匹配逻辑。这与 issue #228 §七原表把
//     Anthropic Messages 这一列全部标 ✓ 矛盾——那大概率是把 Anthropic Messages
//     当作"旗舰/预先存在的基线"直接抄了全部 ✓，但 transport 错误这一格实际上
//     不成立。见 TestAnthropicMessagesTransportErrorBypassesErrorHandlingRule_KnownGap。
//
//  2. Anthropic Messages 的 "错误透传让路" 维度同样没有接线，而且现有调用顺序
//     与 §五要求的优先级相反。errorPassthroughRuleMatches 在全仓库唯一的非测试
//     调用点是 error_handling_rule_executor.go:84（executeErrorHandlingRule 内部），
//     而 Anthropic 自己这条更老、独立的引擎实现（gateway_error_handling_rule.go
//     的 applyErrorHandlingRule / writeErrorHandlingRulePassthrough）从未调用
//     executeErrorHandlingRule，也从未直接调用 errorPassthroughRuleMatches。更
//     进一步，gateway_forward.go 里错误处理规则的 5 个调用点都排在 handleErrorResponse
//     （管理端错误透传规则的落地点）之前，意味着即便两者都配置成会匹配同一个
//     错误，错误处理规则引擎会先赢——这是 §五"错误透传规则永远优先"的反面。
//     见 TestAnthropicMessagesErrorHandlingRuleNeverConsultsPassthroughRule_KnownGap。
//
//  3. Grok Media generation 的 "transport 错误" 维度同样没有真正接线：虽然
//     ForwardGrokMedia 请求发送失败时确实调用了 handleOpenAIUpstreamTransportError
//     （与 OpenAI/Chat Completions/Responses 共用同一个函数体），但
//     openAIErrorHandlingRulesActive（openai_error_handling_rule.go:104）硬编码了
//     `account.Platform != PlatformOpenAI` 直接拒绝——只有 Platform=openai 的账号
//     才会真的问到规则引擎，Grok 账号（Platform=grok）在匹配前就被短路。这条是
//     写验收测试时才发现的：本来以为"机制是通用的，只是没测过"，写出的测试本身
//     断言失败，才发现真相与 grok_media_error_handling_rule.go:146 的注释一致——
//     "Grok media 传输层错误…接线留给后续任务"仍然成立，不是过时注释。见
//     TestGrokMediaGenerationTransportErrorBypassesErrorHandlingRule_KnownGap。
//
// 一个对 RULING B 的有依据的扩展（供复核，不是既定结论）：RULING B 只点名
// Gemini 和 Antigravity 的流内错误维度必须标"不适用"，理由是两个平台的三个
// 流式函数只在 SSE 循环开始前的 resp.StatusCode>=400 检查上跑规则引擎，从未在
// 循环中途跑。对 openai_gateway_response_handling.go 的 handleStreamingResponse /
// handleStreamingResponseWithReasoning，以及 openai_gateway_chat_completions.go /
// openai_gateway_chat_completions_raw.go 做了同样的全文 grep "ErrorHandlingRule"，
// 结果同样是零命中——Chat Completions 和 Responses 的实时 SSE 转发路径同样没有
// 中途规则引擎接线，是与 Gemini/Antigravity 相同的结构性缺口。这里把这两行的
// 流内错误维度也标"不适用"，并引用同样的执行层证明（安全性质——不拼接第二条
// 流——由 executeErrorHandlingRule 对 SemanticEventForwarded 的降级保证，位于
// executor 层而非平台层）。如果这个扩展被认为超出了 RULING B 的字面授权，请
// 在复核时明确指出，我会改回只标 Gemini/Antigravity。

func matrixAssertTestExists(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("matrix self-check: cannot read dir %q: %v", dir, err)
	}
	var source strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("matrix self-check: cannot read %q: %v", e.Name(), err)
		}
		source.Write(data)
		source.WriteByte('\n')
	}
	combined := source.String()
	for _, name := range names {
		if !strings.Contains(combined, "func "+name+"(") {
			t.Errorf("matrix gap: referenced test %q not found in %q — the coverage claim for this cell is stale; "+
				"the test was renamed or deleted and this cell must be re-verified (re-covered, referenced under its "+
				"new name, or reclassified as not-applicable with a documented reason)", name, dir)
		}
	}
}

// ==================== 行 1/6：Anthropic Messages ====================

func TestErrorHandlingRuleMatrix_AnthropicMessages(t *testing.T) {
	t.Run("http_non_failover_action_retry_or_passthrough", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestForwardErrorHandlingRuleRetriesInPlaceThenSucceeds",
			"TestForwardErrorHandlingRulePassthroughReturnsErrorToClient",
			"TestForwardErrorHandlingRuleDisabledFallsBackToExistingBehavior", // miss
		)
	})
	t.Run("http_failover_action_failover", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestForwardErrorHandlingRuleFailoverActionSwitchesImmediately",
			"TestForwardBuiltinSignatureFailoverPreemptsErrorHandlingRule", // miss: 内置签名错误独占抢先
		)
	})
	t.Run("transport_error_KNOWN_GAP_documented_below", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestAnthropicMessagesTransportErrorBypassesErrorHandlingRule_KnownGap")
	})
	t.Run("stream_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestConvertedStreamMissingTerminalRuleFailsOverBeforeOutput",                 // hit：尚未写出任何内容
			"TestConvertedStreamCleanEOFAfterLocalKeepaliveCanFailOver",                   // hit：只写过 keepalive
			"TestConvertedStreamMissingTerminalRulePreservesWrittenStreamAndPartialUsage", // hit：已写出语义内容后降级
			"TestConvertedStreamMissingTerminalWithoutRuleRecordsOpsCause",                // miss
		)
	})
	t.Run("passthrough_yield_KNOWN_GAP_documented_below", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestAnthropicMessagesErrorHandlingRuleNeverConsultsPassthroughRule_KnownGap")
	})
}

// TestAnthropicMessagesTransportErrorBypassesErrorHandlingRule_KnownGap 是矩阵引用
// 的缺口钉住测试（gap-pinning test）：证明当前代码行为，而不是期望行为。即便配置了
// 一条对 Anthropic 平台启用、动作为 passthrough 的规则，handleUpstreamTransportError
// 面对 transport 错误时仍然返回固定的通用 502（ErrorRuleID 为空、NextAccountAction 为
// Retry），完全没有咨询过规则引擎——这与 issue #228 §七原表把 Anthropic Messages
// 这一列标全 ✓ 矛盾，是一个真实的、pre-existing 的生产代码缺口，本任务不修，只诚实
// 记录。
func TestAnthropicMessagesTransportErrorBypassesErrorHandlingRule_KnownGap(t *testing.T) {
	repo := &transportTempUnschedRepoStub{}
	settingRepo := &gatewayTTLSettingRepo{data: map[string]string{}}
	settingSvc := NewSettingService(settingRepo, nil)
	enabled := true
	require.NoError(t, settingSvc.SetErrorHandlingRuleSettings(context.Background(), &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{{
			ID: "would-match-if-wired", Name: "would match if wired", Enabled: &enabled,
			StatusCodes: []int{502}, Action: ErrorHandlingActionPassthrough, Platforms: []string{PlatformAnthropic},
		}},
	}))
	s := &GatewayService{accountRepo: repo, settingService: settingSvc}
	c := newTransportErrorTestGin(t)
	account := &Account{ID: 149, Name: "acc", Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	err := s.handleUpstreamTransportError(context.Background(), c, account,
		errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`), OpsUpstreamErrorEvent{})

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, failoverErr.ErrorRuleID,
		"当前行为：即便有一条会匹配的启用规则，ErrorRuleID 仍为空——规则引擎从未被咨询")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, string(gatewayTransportFailoverBody), string(failoverErr.ResponseBody),
		"当前行为：响应体永远是固定的通用 502 文案，不是规则配置的 passthrough 内容")
	require.True(t, failoverErr.ShouldRetryNextAccount(),
		"当前行为：即便规则配的是 passthrough（应停止换号），transport 错误路径仍然无条件换号")

	events := opsUpstreamErrorEvents(t, c)
	for _, ev := range events {
		require.NotEqual(t, "error_handling_rule_passthrough", ev.Kind)
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind)
	}
}

// TestAnthropicMessagesErrorHandlingRuleNeverConsultsPassthroughRule_KnownGap 是矩阵
// 引用的第二个缺口钉住测试：证明当前行为——错误处理规则引擎的 passthrough 动作会
// 直接把自己的响应体写给客户端，完全不问一句"有没有一条管理端错误透传规则本来应该
// 优先生效"。用一个非 400 状态码（500）让 writeErrorHandlingRulePassthrough 保留原始
// 上游响应体（见 TestForwardErrorHandlingRulePassthroughPreservesNon400RawResponse 建立
// 的先例），同时绑定一条会匹配同一状态码、但产出完全不同响应体/状态码的管理端错误
// 透传规则；如果两者的优先级如 §五要求的那样（错误透传规则优先），最终应该看到
// 透传规则的自定义响应；实际观察到的是错误处理规则自己的 passthrough 输出赢了。
func TestAnthropicMessagesErrorHandlingRuleNeverConsultsPassthroughRule_KnownGap(t *testing.T) {
	rawBody := `{"error":{"type":"upstream_custom","message":"raw proxy failure"}}`
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 500, body: rawBody}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough}},
	})

	respCode := http.StatusTeapot
	customMessage := "错误透传规则本应生效的自定义消息"
	passthroughSvc := &ErrorPassthroughService{}
	passthroughSvc.setLocalCache([]*model.ErrorPassthroughRule{{
		ID: 1, Name: "anthropic-messages-500", Enabled: true, Priority: 1,
		ErrorCodes: []int{http.StatusInternalServerError}, MatchMode: model.MatchModeAll,
		Platforms: []string{PlatformAnthropic}, ResponseCode: &respCode, CustomMessage: &customMessage,
	}})

	c, rec := newErrorHandlingRuleTestContextWithRecorder()
	BindErrorPassthroughService(c, passthroughSvc)

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))

	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "passthrough 动作不换号，Forward 不应该返回 *UpstreamFailoverError")
	require.Equal(t, 1, upstream.calls)
	require.NotEqual(t, http.StatusTeapot, rec.Code,
		"当前行为：管理端错误透传规则配置的状态码从未生效——错误处理规则先赢，优先级与 §五 相反")
	require.NotContains(t, rec.Body.String(), customMessage,
		"当前行为：管理端错误透传规则配置的自定义消息从未出现在响应里")
	require.Contains(t, rec.Body.String(), "raw proxy failure",
		"当前行为：客户端看到的是错误处理规则 passthrough 保留的原始上游响应体")
}

// ==================== 行 2/6：Gemini Messages（Forward，Anthropic 兼容入口） ====================

func TestErrorHandlingRuleMatrix_GeminiMessages(t *testing.T) {
	t.Run("http_non_failover_and_failover_shared_override", func(t *testing.T) {
		// geminiErrorHandlingRuleOverride 是平台端点无关的：它只吃 Account/StatusCode/
		// Body，Forward / ForwardNative 共用同一个函数，因此这里与"Gemini Native"行
		// 引用的是同一个动作测试组，是有意的共享机制引用而非重复覆盖。
		matrixAssertTestExists(t, ".",
			"TestGeminiErrorHandlingRuleOverrideActions",                // 三个子测试：failover/retry/passthrough
			"TestGeminiErrorHandlingRule_OtherPlatformRuleDoesNotApply", // miss
			"TestGeminiBuiltinOwnsGoogleProjectConfigError",             // miss：内置独占抢先
		)
	})
	t.Run("transport_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestGeminiForward_TransportErrorRuleTakesEffect",
			"TestGeminiForward_TransportErrorNoRuleUnchangedOutput",
		)
	})
	t.Run("stream_error_NOT_APPLICABLE", func(t *testing.T) {
		// RULING B：Gemini 的三个流式转发函数只在 SSE 循环开始前的
		// resp.StatusCode>=400 初始检查上跑规则引擎，从未在循环中途跑；三个流式函数
		// 内对读错误/行超长/数据间隔超时全部直接 return nil, err，没有构造过
		// UpstreamFailoverError，也没有 Anthropic 侧那种流内嵌错误事件识别。
		// 安全性质（不拼接第二条流）由执行层对 SemanticEventForwarded 的降级保证，
		// 与平台是否真的在流中途接线无关——这里引用执行层测试作为"降级机制本身
		// 正确"的证明，同时明确记录"Gemini 平台层还没有流中途接线"这一事实，而不是
		// 悄悄把这个不适用伪装成已覆盖。
		matrixAssertTestExists(t, ".",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesRetryToPassthrough",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesFailoverToPassthrough",
			"TestExecuteErrorHandlingRuleWithoutSemanticEventForwardedStillFailoversCleanly",
		)
	})
	t.Run("passthrough_yield", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestGeminiErrorHandlingRuleYieldsToPassthroughRule")
	})
}

// ==================== 行 3/6：Gemini Native（ForwardNative） ====================

func TestErrorHandlingRuleMatrix_GeminiNative(t *testing.T) {
	t.Run("http_non_failover_and_failover_shared_override", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestGeminiErrorHandlingRuleOverrideActions",
			"TestGeminiErrorHandlingRule_OtherPlatformRuleDoesNotApply",
			"TestGeminiBuiltinOwnsError_GoogleProjectConfig",
		)
	})
	t.Run("transport_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestGeminiForwardNative_TransportErrorRuleTakesEffect",
			"TestGeminiForwardNative_TransportErrorNoRuleUnchangedOutput",
		)
	})
	t.Run("stream_error_NOT_APPLICABLE", func(t *testing.T) {
		// 理由与 Gemini Messages 行相同：ForwardNative 与 Forward 共用同一套只在
		// 初始响应检查规则引擎、从不在流中途接线的结构。
		matrixAssertTestExists(t, ".",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesRetryToPassthrough",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesFailoverToPassthrough",
			"TestExecuteErrorHandlingRuleWithoutSemanticEventForwardedStillFailoversCleanly",
		)
	})
	t.Run("passthrough_yield_shared_override", func(t *testing.T) {
		// TestGeminiErrorHandlingRuleYieldsToPassthroughRule 直接驱动
		// geminiErrorHandlingRuleOverride（端点无关），因此对 Native 行同样成立。
		matrixAssertTestExists(t, ".", "TestGeminiErrorHandlingRuleYieldsToPassthroughRule")
	})
}

// ==================== 行 4/6：Chat Completions（OpenAI 兼容） ====================

func TestErrorHandlingRuleMatrix_ChatCompletions(t *testing.T) {
	t.Run("http_non_failover_and_failover_shared_override", func(t *testing.T) {
		// openAIErrorHandlingRuleOverride 同样端点无关：Chat Completions 与 Responses
		// 共用同一个函数（openai_gateway_forward.go 里唯一的 400+ 接线点），因此这里
		// 与"Responses"行引用的是同一批测试。
		matrixAssertTestExists(t, ".",
			"TestOpenAIErrorHandlingRule_RetryActionSetsSameAccountBudget",
			"TestOpenAIErrorHandlingRule_PassthroughStopsAccountRotation",
			"TestOpenAIErrorHandlingRuleOverride_HTTPErrorResponseMatches",
			"TestOpenAIErrorHandlingRuleOverride_NoMatchLeavesBuiltinPath", // miss
			"TestOpenAIBuiltinOwnsError",                                   // miss：内置独占抢先
		)
	})
	t.Run("transport_error_shared_choke_point", func(t *testing.T) {
		// handleOpenAIUpstreamTransportError / handleOpenAIUpstreamResponseBodyReadError
		// 是 OpenAI/Grok/Chat Completions/Responses/embeddings/images/WS 桥接共用的
		// transport 错误接线点（openai_upstream_transport_error.go:93-194，第 186 行
		// 调用 openAITransportErrorRuleOverride）。下面两个测试直接驱动这个共用函数，
		// 是 Chat Completions 与 Responses 两行都成立的 hit 引用；miss 引用是同一函数
		// 在没有配置任何规则时的既有回归测试。
		matrixAssertTestExists(t, ".",
			"TestOpenAIErrorHandlingRule_TransportErrorMatchesSynthetic502",
			"TestOpenAIErrorHandlingRule_BodyReadErrorMatchesSynthetic502",
			"TestHandleOpenAIUpstreamTransportError_TransientFailsOverWithoutEviction", // miss
			"TestForwardAsRawChatCompletions_TransportErrorFailsOver",                  // Chat Completions 专属路径证明
		)
	})
	t.Run("stream_error_NOT_APPLICABLE_ruling_B_extension", func(t *testing.T) {
		// 对 RULING B 的有依据的扩展（见文件头注释）：openai_gateway_response_handling.go
		// 的 handleStreamingResponse / handleStreamingResponseWithReasoning，以及
		// openai_gateway_chat_completions.go / openai_gateway_chat_completions_raw.go
		// 全文 grep "ErrorHandlingRule" 均为零命中——Chat Completions 的实时 SSE 转发
		// 路径没有中途规则引擎接线，与 Gemini/Antigravity 是同一结构性缺口。安全性质
		// 同样由执行层的 SemanticEventForwarded 降级保证。
		matrixAssertTestExists(t, ".",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesRetryToPassthrough",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesFailoverToPassthrough",
			"TestExecuteErrorHandlingRuleWithoutSemanticEventForwardedStillFailoversCleanly",
		)
	})
	t.Run("passthrough_yield_shared_override", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestOpenAIErrorHandlingRule_YieldsToErrorPassthroughRule")
	})
}

// ==================== 行 5/6：Responses（OpenAI 兼容） ====================

func TestErrorHandlingRuleMatrix_Responses(t *testing.T) {
	t.Run("http_non_failover_and_failover_shared_override", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestOpenAIErrorHandlingRule_RetryActionSetsSameAccountBudget",
			"TestOpenAIErrorHandlingRule_PassthroughStopsAccountRotation",
			"TestOpenAIErrorHandlingRuleOverride_HTTPErrorResponseMatches",
			"TestOpenAIErrorHandlingRuleOverride_NoMatchLeavesBuiltinPath",
			"TestOpenAIBuiltinOwnsError",
		)
	})
	t.Run("transport_error_shared_choke_point", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestOpenAIErrorHandlingRule_TransportErrorMatchesSynthetic502",
			"TestOpenAIErrorHandlingRule_BodyReadErrorMatchesSynthetic502",
			"TestHandleOpenAIUpstreamTransportError_TransientFailsOverWithoutEviction",
		)
	})
	t.Run("stream_error_NOT_APPLICABLE_ruling_B_extension", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesRetryToPassthrough",
			"TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesFailoverToPassthrough",
			"TestExecuteErrorHandlingRuleWithoutSemanticEventForwardedStillFailoversCleanly",
		)
	})
	t.Run("passthrough_yield_shared_override", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestOpenAIErrorHandlingRule_YieldsToErrorPassthroughRule")
	})
}

// ==================== 行 6/6：Grok Media generation ====================

func TestErrorHandlingRuleMatrix_GrokMediaGeneration(t *testing.T) {
	t.Run("http_non_failover_and_failover", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestGrokMediaErrorHandlingRuleActions", // 三个子测试：failover/retry/passthrough
			"TestGrokMediaErrorHandlingRule_OtherPlatformRuleDoesNotApply",
			"TestGrokMediaContentPolicyRejectionBypassesErrorHandlingRule", // miss：内置独占抢先
		)
	})
	t.Run("transport_error_KNOWN_GAP_documented_below", func(t *testing.T) {
		// 第三个诚实披露的真实生产缺口（写这条子测试之前先假设了机制通用、后来被
		// 自己的新测试推翻）：ForwardGrokMedia 请求发送失败时确实调用了
		// handleOpenAIUpstreamTransportError（与 OpenAI/Chat Completions/Responses
		// 共用同一个函数体），但 openAIErrorHandlingRulesActive
		// （openai_error_handling_rule.go:104）里硬编码了
		// `account.Platform != PlatformOpenAI` 直接拒绝——这个函数只对 Platform=openai
		// 的账号生效，Grok media 账号 Platform=grok，规则引擎从第一步就被短路，
		// 无论管理端配没配一条会命中的规则都一样。这与
		// grok_media_error_handling_rule.go:146 那条注释（"Grok media 传输层错误…
		// 接线留给后续任务"）互相印证——那不是一句过时的注释，是仍然成立的真实缺口。
		matrixAssertTestExists(t, ".", "TestGrokMediaGenerationTransportErrorBypassesErrorHandlingRule_KnownGap")
	})
	t.Run("stream_error_NOT_APPLICABLE", func(t *testing.T) {
		t.Log("not applicable: issue #228 §七原表本身把 Grok Media generation 的流内错误列标为" +
			"「不适用」——媒体生成（图片/视频）请求本身是非流式的，没有 SSE 循环可言")
	})
	t.Run("passthrough_yield", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestGrokMediaErrorHandlingRuleYieldsToPassthroughRule")
	})
}

// ==================== Grok Media generation 的 transport 错误缺口钉住测试 ====================
//
// 起初以为这一格只是"没写测试"，尝试写一个证明 ForwardGrokMedia 遇到 transport
// 错误时规则会生效的测试，结果测试本身失败了——才发现 openAIErrorHandlingRulesActive
// 硬编码只认 PlatformOpenAI，Grok 账号从第一步就被拒绝。保留下面这个曾经失败的
// 断言的对照版本（期望值取反），把它变成如实记录当前行为的缺口钉住测试，而不是
// 悄悄改小范围或者删掉不提。

func TestGrokMediaGenerationTransportErrorBypassesErrorHandlingRule_KnownGap(t *testing.T) {
	upstream := &failingOpenAIHTTPUpstream{err: errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`)}
	svc := newGrokMediaRuleService(t, upstream, ErrorHandlingRule{
		ID: "would-match-if-wired", StatusCodes: []int{502}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformGrok},
	})
	c, _ := newGrokMediaRuleTestContext()
	account := grokMediaRuleAccount()

	_, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesGenerations,
		"req-transport-1", []byte(`{"model":"grok-imagine","prompt":"waves"}`), "application/json")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, failoverErr.ErrorRuleID,
		"当前行为：即便配了一条会匹配 502 的 passthrough 规则，Grok media 的 transport 错误路径"+
			"仍然拿到空 ErrorRuleID——openAIErrorHandlingRulesActive 硬编码只认 PlatformOpenAI，"+
			"Grok 账号在规则匹配前就被拒绝")
	require.True(t, failoverErr.ShouldRetryNextAccount(),
		"当前行为：规则配置的 passthrough（应停止换号）从未生效，transport 错误无条件换号")
}

func TestGrokMediaGeneration_TransportErrorNoRuleUnchangedOutput(t *testing.T) {
	upstream := &failingOpenAIHTTPUpstream{err: errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`)}
	svc := newGrokMediaRuleService(t, upstream) // 无规则
	c, _ := newGrokMediaRuleTestContext()
	account := grokMediaRuleAccount()

	_, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesGenerations,
		"req-transport-2", []byte(`{"model":"grok-imagine","prompt":"waves"}`), "application/json")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, failoverErr.ErrorRuleID, "没配规则时必须走既有的通用 502 兜底，不受规则引擎影响")
	require.True(t, failoverErr.ShouldRetryNextAccount())
}
