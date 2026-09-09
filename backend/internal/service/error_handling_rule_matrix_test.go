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
// Anthropic Messages 使用自己的规则执行层，其调用点同样位于管理端错误透传规则
// 之前；Gemini、Antigravity、OpenAI 和 Grok Media 则通过共享 executor 实现相同的
// “错误处理规则优先”语义。Grok Media 的 transport 错误也已通过具体平台闸门接入。
//
// 流内失败现在是所有实时推理协议的一等接线点：平台循环先解析内嵌错误，或把读取
// 失败、超时、缺失终止事件映射为合成 502，再进入共享执行层。只有 Grok Media
// generation 没有 SSE 流，仍然不适用。下面每一行都引用实际驱动协议循环的测试。

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
	t.Run("transport_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestAnthropicMessagesTransportErrorAppliesErrorHandlingRule")
	})
	t.Run("stream_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestConvertedStreamMissingTerminalRuleFailsOverBeforeOutput",                 // hit：尚未写出任何内容
			"TestConvertedStreamCleanEOFAfterLocalKeepaliveCanFailOver",                   // hit：只写过 keepalive
			"TestConvertedStreamMissingTerminalRulePreservesWrittenStreamAndPartialUsage", // hit：已写出语义内容后降级
			"TestConvertedStreamMissingTerminalWithoutRuleRecordsOpsCause",                // miss
		)
	})
	t.Run("passthrough_wins_over_error_passthrough_rule", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestAnthropicMessagesErrorHandlingRuleWinsOverPassthroughRule")
	})
}

func TestAnthropicMessagesTransportErrorAppliesErrorHandlingRule(t *testing.T) {
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
	require.Equal(t, "would-match-if-wired", failoverErr.ErrorRuleID)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, failoverErr.ShouldRetryNextAccount(), "passthrough 规则必须停止换号")
	require.True(t, failoverErr.SyntheticStatus)
	_, hasStatus := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, hasStatus, "合成 502 只能用于匹配，不能伪装成真实上游状态")

	events := opsUpstreamErrorEvents(t, c)
	require.Equal(t, "error_handling_rule_passthrough", events[len(events)-1].Kind)
	require.Zero(t, events[len(events)-1].UpstreamStatusCode)
}

// 错误处理规则引擎的 passthrough 动作会直接把自己的响应体写给客户端，不再查询
// 是否还有管理端错误透传规则会匹配同一个错误。用一个非 400 状态码（500）
// 让 writeErrorHandlingRulePassthrough 保留原始上游响应体（见
// TestForwardErrorHandlingRulePassthroughPreservesNon400RawResponse 建立的先例），同时
// 绑定一条会匹配同一状态码、但产出完全不同响应体/状态码的管理端错误透传规则；
// 实际观察到的是错误处理规则自己的 passthrough 输出赢了。
//
// 这符合规则引擎全链优先于错误透传规则的统一语义。
func TestAnthropicMessagesErrorHandlingRuleWinsOverPassthroughRule(t *testing.T) {
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
		"当前行为：管理端错误透传规则配置的状态码不生效——错误处理规则先赢，"+
			"2026-09-08 起这正是项目所有者要的次序（规则引擎全链优先）")
	require.NotContains(t, rec.Body.String(), customMessage,
		"当前行为：管理端错误透传规则配置的自定义消息不会出现在响应里")
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
	t.Run("stream_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestGeminiMessagesStreamErrorHandlingRuleHitMissAndPostOutput")
	})
	t.Run("passthrough_wins_over_error_passthrough_rule", func(t *testing.T) {
		// 2026-09-08 起项目所有者反转 #228 非目标：两个机制同时命中时错误处理规则
		// 引擎胜出（旧维度名 passthrough_yield，旧语义正相反——那时是透传规则赢）。
		matrixAssertTestExists(t, ".",
			"TestGeminiErrorHandlingRuleWinsOverPassthroughRule",
			// 同时命中之外，"只有透传规则命中"这条独立分支必须继续被覆盖。
			"TestWriteGeminiMappedError_PassthroughRuleAloneStillApplies",
		)
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
	t.Run("stream_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestGeminiNativeStreamErrorHandlingRuleHitAndMiss")
	})
	t.Run("passthrough_wins_over_error_passthrough_rule_shared_override", func(t *testing.T) {
		// TestGeminiErrorHandlingRuleWinsOverPassthroughRule 直接驱动
		// geminiErrorHandlingRuleOverride（端点无关），因此对 Native 行同样成立。
		matrixAssertTestExists(t, ".",
			"TestGeminiErrorHandlingRuleWinsOverPassthroughRule",
			"TestWriteGeminiMappedError_PassthroughRuleAloneStillApplies",
		)
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
	t.Run("stream_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestOpenAIChatCompletionsStreamErrorHandlingRuleHitMissAndPostOutput")
	})
	t.Run("passthrough_wins_over_error_passthrough_rule_shared_override", func(t *testing.T) {
		// 2026-09-08 起项目所有者反转 #228 非目标：两个机制同时命中时错误处理规则
		// 引擎胜出（旧维度名 passthrough_yield，旧语义正相反）。
		matrixAssertTestExists(t, ".",
			"TestOpenAIErrorHandlingRule_WinsOverErrorPassthroughRule",
			"TestOpenAIHandleErrorResponse_PassthroughRuleAloneStillApplies",
		)
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
	t.Run("stream_error", func(t *testing.T) {
		matrixAssertTestExists(t, ".", "TestOpenAIResponsesStreamErrorHandlingRuleHitAndMiss")
	})
	t.Run("passthrough_wins_over_error_passthrough_rule_shared_override", func(t *testing.T) {
		// 2026-09-08 起项目所有者反转 #228 非目标：两个机制同时命中时错误处理规则
		// 引擎胜出（旧维度名 passthrough_yield，旧语义正相反）。
		matrixAssertTestExists(t, ".",
			"TestOpenAIErrorHandlingRule_WinsOverErrorPassthroughRule",
			"TestOpenAIHandleErrorResponse_PassthroughRuleAloneStillApplies",
		)
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
	t.Run("transport_error", func(t *testing.T) {
		// 具体平台闸门允许 Grok Media 的 transport 错误进入规则引擎。
		matrixAssertTestExists(t, ".", "TestGrokMediaGenerationTransportErrorAppliesErrorHandlingRule")
	})
	t.Run("stream_error_NOT_APPLICABLE", func(t *testing.T) {
		t.Log("not applicable: issue #228 §七原表本身把 Grok Media generation 的流内错误列标为" +
			"「不适用」——媒体生成（图片/视频）请求本身是非流式的，没有 SSE 循环可言")
	})
	t.Run("passthrough_wins_over_error_passthrough_rule", func(t *testing.T) {
		// 2026-09-08 起项目所有者反转 #228 非目标：两个机制同时命中时错误处理规则
		// 引擎胜出（旧维度名 passthrough_yield，旧语义正相反——那时透传规则在
		// handleGrokMediaErrorResponse 里结构性抢在规则引擎之前物理 return，
		// Task 12.5 把这个唯一的反向次序也纠正过来了）。
		matrixAssertTestExists(t, ".",
			"TestGrokMediaErrorHandlingRuleWinsOverPassthroughRule",
			"TestHandleGrokMediaErrorResponse_PassthroughRuleAloneStillApplies",
		)
	})
}

// ==================== Grok Media generation 的 transport 错误接线证明测试 ====================
//
// 具体平台闸门允许 Grok Media 的 transport 错误进入规则引擎；下面直接驱动生产
// 请求路径，确保这一格不是只靠共享 helper 的间接覆盖。

func TestGrokMediaGenerationTransportErrorAppliesErrorHandlingRule(t *testing.T) {
	upstream := &failingOpenAIHTTPUpstream{err: errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`)}
	svc := newGrokMediaRuleService(t, upstream, ErrorHandlingRule{
		ID: "grok-media-transport-passthrough", StatusCodes: []int{502}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformGrok},
	})
	c, _ := newGrokMediaRuleTestContext()
	account := grokMediaRuleAccount()

	_, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointImagesGenerations,
		"req-transport-1", []byte(`{"model":"grok-imagine","prompt":"waves"}`), "application/json")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "grok-media-transport-passthrough", failoverErr.ErrorRuleID,
		"打开 gate 3 后：配了一条会匹配 502 的 passthrough 规则，Grok media 的 transport 错误"+
			"路径现在会命中它——openAIErrorHandlingRulesActive 不再硬编码只认 PlatformOpenAI")
	require.False(t, failoverErr.ShouldRetryNextAccount(),
		"规则配置的 passthrough 必须停止换号，不能再无条件换号")
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
