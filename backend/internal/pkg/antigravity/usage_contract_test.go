//go:build unit

package antigravity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Gemini usageMetadata 只有 cachedContentTokenCount（缓存命中），没有缓存写入的 token 类别：
// 两条转换路径产出的 Claude usage 中 cache_creation_input_tokens 恒为 0，缓存命中计入
// cache_read_input_tokens 并从 input_tokens 中扣除。
func TestGeminiUsageMapping_NoCacheCreationTokens(t *testing.T) {
	const geminiBody = `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":30,"thoughtsTokenCount":5}}`

	t.Run("non-stream", func(t *testing.T) {
		_, usage, err := TransformGeminiToClaude([]byte(geminiBody), "gemini-3.1-pro-preview")
		require.NoError(t, err)
		require.NotNil(t, usage)
		require.Equal(t, 70, usage.InputTokens)
		require.Equal(t, 25, usage.OutputTokens)
		require.Equal(t, 30, usage.CacheReadInputTokens)
		require.Zero(t, usage.CacheCreationInputTokens)
	})

	t.Run("stream", func(t *testing.T) {
		p := NewStreamingProcessor("gemini-3.1-pro-preview")
		out := p.ProcessLine(`data: {"response":` + geminiBody + `}`)
		require.True(t, strings.Contains(string(out), `"message_start"`))
		require.NotContains(t, string(out), `"cache_creation_input_tokens"`, "message_start 的 usage 不应携带缓存写入分项")
		_, usage := p.Finish()
		require.NotNil(t, usage)
		require.Equal(t, 70, usage.InputTokens)
		require.Equal(t, 30, usage.CacheReadInputTokens)
		require.Zero(t, usage.CacheCreationInputTokens)
	})
}

// 部分中转上游会先行扣除缓存再回传 promptTokenCount（例如 promptTokenCount=0、
// cachedContentTokenCount=299008）。两条转换路径都不得再减一次得到负的 input_tokens，
// 否则计费侧会产生负费用。
func TestGeminiUsageMapping_UpstreamPreSubtractedCache(t *testing.T) {
	const geminiBody = `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":789,"cachedContentTokenCount":299008}}`

	t.Run("non-stream", func(t *testing.T) {
		_, usage, err := TransformGeminiToClaude([]byte(geminiBody), "gemini-3.8-flash")
		require.NoError(t, err)
		require.NotNil(t, usage)
		require.Equal(t, 0, usage.InputTokens)
		require.Equal(t, 789, usage.OutputTokens)
		require.Equal(t, 299008, usage.CacheReadInputTokens)
	})

	t.Run("stream", func(t *testing.T) {
		p := NewStreamingProcessor("gemini-3.8-flash")
		out := p.ProcessLine(`data: {"response":` + geminiBody + `}`)
		require.True(t, strings.Contains(string(out), `"message_start"`))
		require.NotContains(t, string(out), `"input_tokens":-`, "message_start 的 usage 不得出现负的 input_tokens")
		_, usage := p.Finish()
		require.NotNil(t, usage)
		require.Equal(t, 0, usage.InputTokens)
		require.Equal(t, 299008, usage.CacheReadInputTokens)
	})
}

func TestGeminiUsageMetadata_NetInputTokens(t *testing.T) {
	tests := []struct {
		name           string
		prompt, cached int
		want           int
	}{
		{"官方口径", 100, 30, 70},
		{"无缓存", 100, 0, 100},
		{"全部命中缓存", 300, 300, 0},
		{"上游已扣除缓存", 0, 299008, 0},
		{"上游已扣除缓存且有未命中输入", 120, 4096, 120},
		{"负数 prompt 兜底", -1, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &GeminiUsageMetadata{PromptTokenCount: tt.prompt, CachedContentTokenCount: tt.cached}
			require.Equal(t, tt.want, m.NetInputTokens())
		})
	}
}
