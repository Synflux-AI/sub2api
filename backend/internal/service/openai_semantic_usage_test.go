package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// 中转上游回「Anthropic 形状 + OpenAI 语义」：外层 input_tokens 是含缓存的 prompt
// 总量，真实净输入只能从 billing_usage.openai_usage 还原。
const openAISemanticMessageStart = `{"type":"message_start","message":{"type":"message","model":"FW-Kimi-K3","usage":{` +
	`"input_tokens":50722,"cache_creation_input_tokens":0,"cache_read_input_tokens":50457,"output_tokens":316,` +
	`"claude_cache_creation_5_m_tokens":0,"claude_cache_creation_1_h_tokens":0,` +
	`"billing_usage":{"source":"oai_chat","semantic":"openai","openai_usage":{` +
	`"prompt_tokens":50722,"completion_tokens":316,"total_tokens":51038,` +
	`"prompt_tokens_details":{"cached_tokens":50457,"text_tokens":0,"audio_tokens":0,"image_tokens":0},` +
	`"input_tokens":0,"output_tokens":0}}}}}`

const openAISemanticNonStreamingBody = `{"type":"message","model":"FW-Kimi-K3","usage":{` +
	`"input_tokens":50722,"cache_creation_input_tokens":0,"cache_read_input_tokens":50457,"output_tokens":316,` +
	`"billing_usage":{"source":"oai_chat","semantic":"openai","openai_usage":{` +
	`"prompt_tokens":50722,"completion_tokens":316,"total_tokens":51038,` +
	`"prompt_tokens_details":{"cached_tokens":50457}}}}}`

func TestParseSSEUsagePassthroughRestoresOpenAISemanticInput(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(openAISemanticMessageStart, usage, false)

	require.Equal(t, 265, usage.InputTokens, "净输入 = prompt_tokens - cached_tokens")
	require.Equal(t, 50457, usage.CacheReadInputTokens)
	require.Equal(t, 0, usage.CacheCreationInputTokens)
}

func TestParseClaudeUsageFromResponseBodyRestoresOpenAISemanticInput(t *testing.T) {
	usage := parseClaudeUsageFromResponseBody([]byte(openAISemanticNonStreamingBody), false)

	require.Equal(t, 265, usage.InputTokens)
	require.Equal(t, 50457, usage.CacheReadInputTokens)
	require.Equal(t, 316, usage.OutputTokens)
}

// CN 原生 Anthropic 直通把 ClaudeUsage 合并成 OpenAI 网关的「含缓存总输入」，
// RecordUsage 再拆回互斥桶。端到端结果必须是净输入 265 而不是 50722。
func TestClaudeUsageToOpenAIUsageRoundTripsOpenAISemanticInput(t *testing.T) {
	usage := parseClaudeUsageFromResponseBody([]byte(openAISemanticNonStreamingBody), false)
	openAIUsage := claudeUsageToOpenAIUsage(usage)

	require.Equal(t, 50722, openAIUsage.InputTokens, "合并后应等于 prompt 总量")

	billed := openAIUsage.InputTokens - openAIUsage.CacheReadInputTokens - openAIUsage.CacheCreationInputTokens
	require.Equal(t, 265, billed, "RecordUsage 拆桶后按净输入计费")
}

func TestOpenAISemanticUsageLeavesOtherUpstreamsUnchanged(t *testing.T) {
	tests := []struct {
		name              string
		data              string
		wantInput         int
		wantCacheRead     int
		wantCacheCreation int
	}{
		{
			// 原生 Anthropic：没有 billing_usage，input_tokens 本就是净输入。
			name:          "原生 Anthropic 不改写",
			data:          `{"type":"message_start","message":{"usage":{"input_tokens":265,"cache_read_input_tokens":50457,"output_tokens":0}}}`,
			wantInput:     265,
			wantCacheRead: 50457,
		},
		{
			// 上游声明 anthropic 口径：即便带了 openai_usage 也不能减。
			name:          "semantic=anthropic 不改写",
			data:          `{"type":"message_start","message":{"usage":{"input_tokens":265,"cache_read_input_tokens":50457,"billing_usage":{"semantic":"anthropic","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":50457}}}}}}`,
			wantInput:     265,
			wantCacheRead: 50457,
		},
		{
			// Kimi 官方的扁平 prompt_tokens 形状：既有守卫继续生效。
			name:          "扁平 prompt_tokens 仍走既有守卫",
			data:          `{"type":"message_delta","usage":{"input_tokens":1200,"output_tokens":30,"prompt_tokens":1200,"prompt_tokens_details":{"cached_tokens":800}}}`,
			wantInput:     400,
			wantCacheRead: 800,
		},
		{
			// 声明了口径但 openai_usage 不可用时退回原语义，不能把输入清零。
			name:          "openai_usage 缺 prompt_tokens 时退回原语义",
			data:          `{"type":"message_start","message":{"usage":{"input_tokens":265,"cache_read_input_tokens":50457,"billing_usage":{"semantic":"openai","openai_usage":{"completion_tokens":316}}}}}`,
			wantInput:     265,
			wantCacheRead: 50457,
		},
		{
			// 缓存写入也要从总量里扣掉，否则 cache_creation 被重复计费。
			name:              "缓存写入同样从总量扣除",
			data:              `{"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_creation_input_tokens":600,"cache_read_input_tokens":300,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":300}}}}}}`,
			wantInput:         100,
			wantCacheRead:     300,
			wantCacheCreation: 600,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &ClaudeUsage{}
			parseSSEUsagePassthrough(tt.data, usage, false)
			require.Equal(t, tt.wantInput, usage.InputTokens)
			require.Equal(t, tt.wantCacheRead, usage.CacheReadInputTokens)
			require.Equal(t, tt.wantCacheCreation, usage.CacheCreationInputTokens)
		})
	}
}

// Anthropic 平台转换路径（非 passthrough）用的是另一套 map 解析器，同样要认口径声明。
func TestExtractSSEUsagePatchRestoresOpenAISemanticInput(t *testing.T) {
	svc := &GatewayService{}

	tests := []struct {
		name              string
		event             map[string]any
		wantInput         int
		wantCacheRead     int
		wantCacheCreation int
	}{
		{
			name: "message_start 声明 openai 口径",
			event: map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"usage": map[string]any{
						"input_tokens":            float64(50722),
						"cache_read_input_tokens": float64(50457),
						"billing_usage": map[string]any{
							"semantic": "openai",
							"openai_usage": map[string]any{
								"prompt_tokens":         float64(50722),
								"prompt_tokens_details": map[string]any{"cached_tokens": float64(50457)},
							},
						},
					},
				},
			},
			wantInput:     265,
			wantCacheRead: 50457,
		},
		{
			name: "message_delta 声明 openai 口径",
			event: map[string]any{
				"type": "message_delta",
				"usage": map[string]any{
					"output_tokens": float64(316),
					"billing_usage": map[string]any{
						"semantic": "openai",
						"openai_usage": map[string]any{
							"prompt_tokens":         float64(50722),
							"prompt_tokens_details": map[string]any{"cached_tokens": float64(50457)},
						},
					},
				},
			},
			wantInput:     265,
			wantCacheRead: 50457,
		},
		{
			name: "无 billing_usage 时保持原语义",
			event: map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"usage": map[string]any{
						"input_tokens":            float64(265),
						"cache_read_input_tokens": float64(50457),
					},
				},
			},
			wantInput:     265,
			wantCacheRead: 50457,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &ClaudeUsage{}
			mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(tt.event), false)
			require.Equal(t, tt.wantInput, usage.InputTokens)
			require.Equal(t, tt.wantCacheRead, usage.CacheReadInputTokens)
			require.Equal(t, tt.wantCacheCreation, usage.CacheCreationInputTokens)
		})
	}
}

// ---------------------------------------------------------------------------
// 多事件流：单事件用例抓不到 message_start → message_delta 的覆盖问题
// ---------------------------------------------------------------------------

const openAISemanticStartEvent = `{"type":"message_start","message":{"usage":{` +
	`"input_tokens":50722,"cache_creation_input_tokens":0,"cache_read_input_tokens":50457,` +
	`"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":50722,` +
	`"prompt_tokens_details":{"cached_tokens":50457}}}}}}`

func TestParseSSEUsagePassthroughKeepsRestoredInputAcrossDeltas(t *testing.T) {
	tests := []struct {
		name  string
		delta string
	}{
		{
			// 上游在 delta 里重复外层总量、且不带声明——裸值不能覆盖已还原的净输入。
			name:  "delta 重复总量且无声明",
			delta: `{"type":"message_delta","usage":{"input_tokens":50722,"output_tokens":316,"cache_read_input_tokens":50457}}`,
		},
		{
			name:  "delta 只回 output",
			delta: `{"type":"message_delta","usage":{"output_tokens":316}}`,
		},
		{
			name:  "delta 重复声明",
			delta: `{"type":"message_delta","usage":{"output_tokens":316,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":50457}}}}}`,
		},
		{
			// 声明里 cached_tokens 缺失/为 0 时，种子要取累计的 cache_read。
			name:  "delta 声明里 cached_tokens 为 0",
			delta: `{"type":"message_delta","usage":{"output_tokens":316,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":0}}}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &ClaudeUsage{}
			parseSSEUsagePassthrough(openAISemanticStartEvent, usage, false)
			require.Equal(t, 265, usage.InputTokens, "message_start 应已还原净输入")
			parseSSEUsagePassthrough(tt.delta, usage, false)
			require.Equal(t, 265, usage.InputTokens, "delta 不得把净输入打回总量")
			require.Equal(t, 50457, usage.CacheReadInputTokens)
		})
	}
}

// 原生 Anthropic 的 delta 语义必须保持原样：input_tokens 该覆盖就覆盖。
func TestParseSSEUsagePassthroughNativeAnthropicDeltaStillOverwrites(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50457}}}`, usage, false)
	parseSSEUsagePassthrough(`{"type":"message_delta","usage":{"input_tokens":265,"output_tokens":316}}`, usage, false)

	require.Equal(t, 265, usage.InputTokens, "无声明时 delta 覆盖行为不变")
	require.Equal(t, 50457, usage.CacheReadInputTokens)
	require.False(t, usage.OpenAISemanticDeclared)
}

func TestExtractSSEUsagePatchKeepsRestoredInputAcrossDeltas(t *testing.T) {
	svc := &GatewayService{}
	decode := func(raw string) map[string]any {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &event))
		return event
	}

	t.Run("delta 缺 cache_creation 时不得少扣", func(t *testing.T) {
		usage := &ClaudeUsage{}
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(`{"type":"message_start","message":{"usage":{"input_tokens":50722,"cache_creation_input_tokens":1000,"cache_read_input_tokens":49457,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":49457}}}}}}`)), false)
		require.Equal(t, 265, usage.InputTokens)
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(`{"type":"message_delta","usage":{"output_tokens":316,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":49457}}}}}`)), false)
		require.Equal(t, 265, usage.InputTokens, "cache_creation 不得被重复计入 input")
		require.Equal(t, 1000, usage.CacheCreationInputTokens)
	})

	t.Run("delta 裸 input_tokens 不得打回总量", func(t *testing.T) {
		usage := &ClaudeUsage{}
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(`{"type":"message_start","message":{"usage":{"input_tokens":50722,"cache_read_input_tokens":50457,"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":50457}}}}}}`)), false)
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(`{"type":"message_delta","usage":{"input_tokens":50722,"output_tokens":316}}`)), false)
		require.Equal(t, 265, usage.InputTokens)
	})

	t.Run("原生 Anthropic delta 覆盖行为不变", func(t *testing.T) {
		usage := &ClaudeUsage{}
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50457}}}`)), false)
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(`{"type":"message_delta","usage":{"input_tokens":265,"output_tokens":316}}`)), false)
		require.Equal(t, 265, usage.InputTokens)
		require.Equal(t, 50457, usage.CacheReadInputTokens)
	})
}

// ---------------------------------------------------------------------------
// 第三套解析器：apicompat.AnthropicUsage / mergeAnthropicUsage
// ---------------------------------------------------------------------------

func TestMergeAnthropicUsageHonorsBillingUsageDeclaration(t *testing.T) {
	var start apicompat.AnthropicUsage
	require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":50722,"cache_creation_input_tokens":0,"cache_read_input_tokens":50457,`+
		`"billing_usage":{"semantic":"openai","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":50457}}}}`), &start))
	require.NotNil(t, start.BillingUsage, "billing_usage 必须能反序列化出来")

	usage := &ClaudeUsage{}
	mergeAnthropicUsage(usage, start, false)
	require.Equal(t, 265, usage.InputTokens)
	require.Equal(t, 50457, usage.CacheReadInputTokens)

	var delta apicompat.AnthropicUsage
	require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":50722,"output_tokens":316}`), &delta))
	mergeAnthropicUsage(usage, delta, false)
	require.Equal(t, 265, usage.InputTokens, "裸 input_tokens 不得打回总量")
	require.Equal(t, 316, usage.OutputTokens)
}

func TestMergeAnthropicUsageLeavesNativeAnthropicUnchanged(t *testing.T) {
	var native apicompat.AnthropicUsage
	require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":265,"output_tokens":316,"cache_creation_input_tokens":120,"cache_read_input_tokens":50457}`), &native))
	require.Nil(t, native.BillingUsage)

	usage := &ClaudeUsage{}
	mergeAnthropicUsage(usage, native, false)
	require.Equal(t, 265, usage.InputTokens)
	require.Equal(t, 316, usage.OutputTokens)
	require.Equal(t, 120, usage.CacheCreationInputTokens)
	require.Equal(t, 50457, usage.CacheReadInputTokens)
	require.False(t, usage.OpenAISemanticDeclared)
}

// ---------------------------------------------------------------------------
// 线上契约：新增字段不得出现在任何回给客户端的 JSON 里
// ---------------------------------------------------------------------------

func TestUsageStructsDoNotLeakNewFieldsOnTheWire(t *testing.T) {
	claudeUsage, err := json.Marshal(ClaudeUsage{InputTokens: 265, OutputTokens: 316, OpenAISemanticDeclared: true})
	require.NoError(t, err)
	require.NotContains(t, string(claudeUsage), "OpenAISemanticDeclared")
	require.NotContains(t, string(claudeUsage), "openai_semantic")

	// 各 *_anthropic_native.go 的出站 usage 都是这样的新建字面量。
	outbound, err := json.Marshal(apicompat.AnthropicUsage{
		InputTokens: 265, OutputTokens: 316, CacheReadInputTokens: 50457,
	})
	require.NoError(t, err)
	require.NotContains(t, string(outbound), "billing_usage")
}

// ---------------------------------------------------------------------------
// 上游不声明口径：靠账号标注 upstream_usage_openai_semantic 兜底还原
// ---------------------------------------------------------------------------

// 中转回「Anthropic 形状 + OpenAI 语义」但不带 billing_usage：input_tokens 是含
// 缓存的 prompt 总量，净输入只能用 75661 - 75264 = 397 还原（与上游账单一致）。
const undeclaredOpenAISemanticStart = `{"type":"message_start","message":{"type":"message","model":"kimi-k2-thinking","usage":{` +
	`"input_tokens":75661,"cache_creation_input_tokens":0,"cache_read_input_tokens":75264,"output_tokens":125,` +
	`"claude_cache_creation_5_m_tokens":0,"claude_cache_creation_1_h_tokens":0}}}`

const undeclaredOpenAISemanticBody = `{"type":"message","model":"kimi-k2-thinking","usage":{` +
	`"input_tokens":75661,"cache_creation_input_tokens":0,"cache_read_input_tokens":75264,"output_tokens":125}}`

func TestForceOpenAISemanticRestoresInputWhenUpstreamStaysSilent(t *testing.T) {
	t.Run("流式 message_start", func(t *testing.T) {
		usage := &ClaudeUsage{}
		parseSSEUsagePassthrough(undeclaredOpenAISemanticStart, usage, true)

		require.Equal(t, 397, usage.InputTokens, "净输入 = 总量 - 缓存读取")
		require.Equal(t, 75264, usage.CacheReadInputTokens)
		require.Equal(t, 0, usage.CacheCreationInputTokens)
	})

	t.Run("非流式", func(t *testing.T) {
		usage := parseClaudeUsageFromResponseBody([]byte(undeclaredOpenAISemanticBody), true)

		require.Equal(t, 397, usage.InputTokens)
		require.Equal(t, 75264, usage.CacheReadInputTokens)
		require.Equal(t, 125, usage.OutputTokens)
	})

	t.Run("平台转换路径", func(t *testing.T) {
		svc := &GatewayService{}
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(undeclaredOpenAISemanticStart), &event))

		usage := &ClaudeUsage{}
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(event), true)

		require.Equal(t, 397, usage.InputTokens)
		require.Equal(t, 75264, usage.CacheReadInputTokens)
	})

	t.Run("apicompat 解析器", func(t *testing.T) {
		var src apicompat.AnthropicUsage
		require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":75661,"cache_creation_input_tokens":0,`+
			`"cache_read_input_tokens":75264,"output_tokens":125}`), &src))

		usage := &ClaudeUsage{}
		mergeAnthropicUsage(usage, src, true)

		require.Equal(t, 397, usage.InputTokens)
		require.Equal(t, 75264, usage.CacheReadInputTokens)
		require.Equal(t, 125, usage.OutputTokens)
	})

	// CN 原生直通把 ClaudeUsage 合并成 OpenAI 网关的「含缓存总输入」，
	// RecordUsage 再拆桶；端到端必须落在 397 而不是 75661。
	t.Run("OpenAI 计费主干拆桶", func(t *testing.T) {
		usage := parseClaudeUsageFromResponseBody([]byte(undeclaredOpenAISemanticBody), true)
		openAIUsage := claudeUsageToOpenAIUsage(usage)

		require.Equal(t, 75661, openAIUsage.InputTokens, "合并后应等于 prompt 总量")
		billed := openAIUsage.InputTokens - openAIUsage.CacheReadInputTokens - openAIUsage.CacheCreationInputTokens
		require.Equal(t, 397, billed)
	})
}

// 开关关闭时行为必须与修复前完全一致。
func TestForceOpenAISemanticOffKeepsUpstreamValue(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(undeclaredOpenAISemanticStart, usage, false)

	require.Equal(t, 75661, usage.InputTokens)
	require.Equal(t, 75264, usage.CacheReadInputTokens)
	require.False(t, usage.OpenAISemanticDeclared)
}

// 还原只做一次：delta 里的裸 input_tokens 既不能再扣一遍缓存，也不能把净输入
// 打回总量。
func TestForceOpenAISemanticRestoresOnlyOnce(t *testing.T) {
	t.Run("gjson 解析器", func(t *testing.T) {
		usage := &ClaudeUsage{}
		parseSSEUsagePassthrough(undeclaredOpenAISemanticStart, usage, true)
		parseSSEUsagePassthrough(`{"type":"message_delta","usage":{"input_tokens":75661,"output_tokens":125,"cache_read_input_tokens":75264}}`, usage, true)

		require.Equal(t, 397, usage.InputTokens)
		require.Equal(t, 75264, usage.CacheReadInputTokens)
		require.Equal(t, 125, usage.OutputTokens)
	})

	t.Run("平台转换路径", func(t *testing.T) {
		svc := &GatewayService{}
		decode := func(raw string) map[string]any {
			var event map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &event))
			return event
		}

		usage := &ClaudeUsage{}
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(undeclaredOpenAISemanticStart)), true)
		mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(decode(`{"type":"message_delta","usage":{"input_tokens":75661,"output_tokens":125}}`)), true)

		require.Equal(t, 397, usage.InputTokens)
	})
}

// 上游自己声明了口径时以声明为准，账号标注不得再叠一次减法。
func TestForceOpenAISemanticDoesNotStackOnDeclaredUpstream(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(openAISemanticMessageStart, usage, true)

	require.Equal(t, 265, usage.InputTokens)
	require.Equal(t, 50457, usage.CacheReadInputTokens)
}

// 上游回扁平 prompt_tokens（Kimi 官方形状）时也以它为准，不叠加减法。
func TestForceOpenAISemanticDoesNotStackOnFlatPromptTokens(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(`{"type":"message_start","message":{"usage":{"input_tokens":75661,`+
		`"cache_read_input_tokens":75264,"prompt_tokens":75661}}}`, usage, true)

	require.Equal(t, 397, usage.InputTokens)
	require.Equal(t, 75264, usage.CacheReadInputTokens)
}

// 没有缓存 token 时减法是恒等变换：不改写、也不打标，delta 的覆盖行为保持原样。
func TestForceOpenAISemanticLeavesUncachedRequestsAlone(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(`{"type":"message_start","message":{"usage":{"input_tokens":1200,"output_tokens":0}}}`, usage, true)
	require.Equal(t, 1200, usage.InputTokens)
	require.False(t, usage.OpenAISemanticDeclared)

	parseSSEUsagePassthrough(`{"type":"message_delta","usage":{"input_tokens":1280,"output_tokens":64}}`, usage, true)
	require.Equal(t, 1280, usage.InputTokens, "无缓存时不打标，delta 仍按原语义覆盖")
}

// input_tokens 为 0 时无从判断口径：不能打标，否则后续真正带净输入的事件会被忽略。
func TestForceOpenAISemanticSkipsZeroInput(t *testing.T) {
	usage := &ClaudeUsage{CacheReadInputTokens: 75264}
	require.False(t, forceOpenAISemanticPromptUsage(usage))
	require.Equal(t, 0, usage.InputTokens)
	require.False(t, usage.OpenAISemanticDeclared)
}

// 只给嵌套 5m/1h 明细、没给扁平聚合时，减法要先把缓存写入补齐。
func TestForceOpenAISemanticSubtractsNestedCacheCreation(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(`{"type":"message_start","message":{"usage":{"input_tokens":75661,`+
		`"cache_read_input_tokens":74264,"cache_creation":{"ephemeral_5m_input_tokens":1000,"ephemeral_1h_input_tokens":0}}}}`, usage, true)

	require.Equal(t, 397, usage.InputTokens)
	require.Equal(t, 1000, usage.CacheCreationInputTokens)
}

func TestAccountIsUpstreamUsageOpenAISemantic(t *testing.T) {
	require.False(t, (*Account)(nil).IsUpstreamUsageOpenAISemantic())
	require.False(t, (&Account{}).IsUpstreamUsageOpenAISemantic())
	require.False(t, (&Account{Extra: map[string]any{"upstream_usage_openai_semantic": "true"}}).IsUpstreamUsageOpenAISemantic())
	require.False(t, (&Account{Extra: map[string]any{"upstream_usage_openai_semantic": false}}).IsUpstreamUsageOpenAISemantic())
	require.True(t, (&Account{Extra: map[string]any{"upstream_usage_openai_semantic": true}}).IsUpstreamUsageOpenAISemantic())
}
