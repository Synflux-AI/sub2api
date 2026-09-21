package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiClientRejectsSSEComments(t *testing.T) {
	cases := []struct {
		name string
		hint string
		want bool
	}{
		{"go-genai (Antigravity CLI)", "google-genai-sdk/1.71.0 gl-go/go1.28-20260721-RC03 cl/951519500 +3ebc191975 X:fieldtrack,boringcrypto", true},
		{"python-genai", "google-genai-sdk/1.20.0 gl-python/3.12.4", true},
		{"js-genai tolerates comments", "google-genai-sdk/1.9.0 gl-node/22.3.0", false},
		{"gemini-cli", "GeminiCLI/0.60.0 (darwin; arm64)", false},
		{"curl", "curl/8.7.1", false},
		{"empty", "", false},
		{"case-insensitive", "Google-GenAI-SDK/1.0.0 GL-Go/go1.27", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, geminiClientRejectsSSEComments(tc.hint))
		})
	}
}

func TestDownstreamRejectsSSECommentsReadsBothHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", nil)
	require.False(t, downstreamRejectsSSEComments(c))

	c.Request.Header.Set("X-Goog-Api-Client", "google-genai-sdk/1.71.0 gl-go/go1.28")
	require.True(t, downstreamRejectsSSEComments(c))

	c.Request.Header.Del("X-Goog-Api-Client")
	c.Request.Header.Set("User-Agent", "google-genai-sdk/1.71.0 gl-go/go1.28")
	require.True(t, downstreamRejectsSSEComments(c))

	require.False(t, downstreamRejectsSSEComments(nil))
}

const (
	// 配置校验允许的最小心跳间隔，测试里取最小值缩短观察窗口。
	antigravityStreamKeepaliveIntervalSeconds = 1
	// 空闲观察窗口必须覆盖到第二次 tick：心跳除了 ticker 还有一道「距上次数据 >= keepaliveInterval」
	// 的二次闸门（antigravity_gateway_streaming.go:374），而 lastDataAt 会被上游数据刷新到 ticker
	// 创建之后，第一次 tick 必然差这点偏移够不到闸门。窗口只盖到第一次 tick 的话，断言能不能过
	// 取决于 ticker 投递抖动，CI 负载一高就翻。
	antigravityStreamIdleObserveWindow = 2500 * time.Millisecond
)

// runAntigravityGeminiStreamWithIdle 起一条上游流：先发一个 data 事件，然后空闲 idle 时长再关闭，
// 返回写给下游的全部字节。用来观察空闲期间网关是否发了 ":\n\n" 心跳。
func runAntigravityGeminiStreamWithIdle(t *testing.T, userAgent string, idle time.Duration) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(
		config.GatewayConfig{
			MaxLineSize:             defaultMaxLineSize,
			StreamKeepaliveInterval: antigravityStreamKeepaliveIntervalSeconds,
		},
		nil,
	)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", nil)
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	reader, writer := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
	done := make(chan error, 1)
	go func() {
		_, err := svc.handleGeminiStreamingResponse(c, resp, time.Now())
		done <- err
	}()
	_, err := io.WriteString(
		writer,
		`data: {"response":{"responseId":"resp_1","candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1}}}`+"\n\n",
	)
	require.NoError(t, err)
	time.Sleep(idle)
	// 本仓在这条路径上有「流未见终止事件」守卫（antigravity_gateway_streaming.go:239），
	// 上游原测试直接关流会被判成 missing terminal event。空闲观察期结束后补一个带
	// finishReason 的收尾块，既保留心跳观察窗口又让流正常收尾。
	_, err = io.WriteString(
		writer,
		`data: {"response":{"responseId":"resp_1","candidates":[{"content":{"parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":2}}}`+"\n\n",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, <-done)
	require.NoError(t, reader.Close())
	return recorder.Body.String()
}

func TestAntigravityGeminiStreamKeepsCommentKeepaliveForOrdinaryClients(t *testing.T) {
	out := runAntigravityGeminiStreamWithIdle(t, "curl/8.7.1", antigravityStreamIdleObserveWindow)
	require.Contains(t, out, ":\n\n", "ordinary clients should still get the idle keepalive")
	require.Contains(t, out, `"text":"partial"`)
}

func TestAntigravityGeminiStreamSkipsCommentKeepaliveForGoGenai(t *testing.T) {
	out := runAntigravityGeminiStreamWithIdle(t, "google-genai-sdk/1.71.0 gl-go/go1.28-20260721-RC03", antigravityStreamIdleObserveWindow)
	require.Contains(t, out, `"text":"partial"`)
	for _, event := range strings.Split(out, "\n\n") {
		require.False(t, strings.HasPrefix(event, ":"), "go-genai must never receive an SSE comment event, got %q", event)
	}
}
