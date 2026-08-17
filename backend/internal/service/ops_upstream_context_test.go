package service

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSafeUpstreamURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"strips query", "https://api.anthropic.com/v1/messages?beta=true", "https://api.anthropic.com/v1/messages"},
		{"strips fragment", "https://api.openai.com/v1/responses#frag", "https://api.openai.com/v1/responses"},
		{"strips both", "https://host/path?token=secret#x", "https://host/path"},
		{"no query or fragment", "https://host/path", "https://host/path"},
		{"empty string", "", ""},
		{"whitespace only", "  ", ""},
		{"query before fragment", "https://h/p?a=1#f", "https://h/p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, safeUpstreamURL(tt.input))
		})
	}
}

// TestUpstreamRawMatchBody 覆盖 checkSkipMonitoringForUpstreamEvent 迁出去的
// body 优先级派生逻辑：Detail 优先，为空回退 Message，两者皆空返回空字符串
// （MatchRule 仍应被调用以匹配仅按 ErrorCodes 生效的规则）。
func TestUpstreamRawMatchBody(t *testing.T) {
	tests := []struct {
		name    string
		detail  string
		message string
		want    string
	}{
		{"detail non-empty wins", `{"api_key":"secret"}`, "fallback message", `{"api_key":"secret"}`},
		{"detail empty falls back to message", "", "upstream failed", "upstream failed"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, upstreamRawMatchBody(tt.detail, tt.message))
		})
	}
}

// TestAppendOpsUpstreamErrorMatchesRawBodyButStoresSanitizedDetail 是 Finding 4
// 的端到端回归：appendOpsUpstreamError 必须用脱敏前的原始 Detail 做规则匹配
// （否则被打码的关键字永远命不中 skip_monitoring 规则），但落进
// OpsUpstreamErrorsKey 的事件里的 Detail 必须是脱敏后的值（不能因为要匹配
// 规则就把原文写回日志/ops 字段）。
func TestAppendOpsUpstreamErrorMatchesRawBodyButStoresSanitizedDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	const secret = "sk-live-testkeyword123"
	rule := newNonFailoverPassthroughRule(400, secret, 400, "上游请求失败")
	rule.SkipMonitoring = true

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)

	detail := `{"error":{"message":"denied","api_key":"` + secret + `"}}`
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           PlatformAnthropic,
		UpstreamStatusCode: 400,
		Detail:             detail,
	})

	// 规则必须按原文命中：secret 出现在 Detail 里，脱敏后会被替换为掩码，
	// 如果匹配用的是脱敏后的文本，这条断言会失败。
	v, exists := c.Get(OpsSkipPassthroughKey)
	require.True(t, exists, "skip_monitoring 规则应按原始 Detail 命中")
	skip, _ := v.(bool)
	require.True(t, skip)

	// 落库/日志字段必须是脱敏后的值，不能因为要匹配规则就保留原文。
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.NotContains(t, events[0].Detail, secret)
	require.Contains(t, events[0].Detail, upstreamSensitiveMask)
}
