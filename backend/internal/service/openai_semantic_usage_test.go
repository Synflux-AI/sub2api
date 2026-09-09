package service

import (
	"testing"

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
	parseSSEUsagePassthrough(openAISemanticMessageStart, usage)

	require.Equal(t, 265, usage.InputTokens, "净输入 = prompt_tokens - cached_tokens")
	require.Equal(t, 50457, usage.CacheReadInputTokens)
	require.Equal(t, 0, usage.CacheCreationInputTokens)
}

func TestParseClaudeUsageFromResponseBodyRestoresOpenAISemanticInput(t *testing.T) {
	usage := parseClaudeUsageFromResponseBody([]byte(openAISemanticNonStreamingBody))

	require.Equal(t, 265, usage.InputTokens)
	require.Equal(t, 50457, usage.CacheReadInputTokens)
	require.Equal(t, 316, usage.OutputTokens)
}

// CN 原生 Anthropic 直通把 ClaudeUsage 合并成 OpenAI 网关的「含缓存总输入」，
// RecordUsage 再拆回互斥桶。端到端结果必须是净输入 265 而不是 50722。
func TestClaudeUsageToOpenAIUsageRoundTripsOpenAISemanticInput(t *testing.T) {
	usage := parseClaudeUsageFromResponseBody([]byte(openAISemanticNonStreamingBody))
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
			parseSSEUsagePassthrough(tt.data, usage)
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
			mergeSSEUsagePatch(usage, svc.extractSSEUsagePatch(tt.event))
			require.Equal(t, tt.wantInput, usage.InputTokens)
			require.Equal(t, tt.wantCacheRead, usage.CacheReadInputTokens)
			require.Equal(t, tt.wantCacheCreation, usage.CacheCreationInputTokens)
		})
	}
}
