package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newOpsAttrTestContext(path string, body []byte) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func TestOpsRequestAttributesSetGetAndBound(t *testing.T) {
	c := newOpsAttrTestContext("/v1/responses", nil)
	require.Equal(t, OpsRequestAttributes{}, GetOpsRequestAttributes(c))
	require.Nil(t, OpsRequestElapsedMs(c), "未记录起点时 duration 为 NULL")

	high, priority := "high", "priority"
	SetOpsReasoningEffort(c, &high)
	SetOpsServiceTier(c, &priority)
	SetOpsImageRequest(c, " 2K ", "1536x1024", 3)
	SetOpsImageResponseFormat(c, "URL")
	require.Equal(t, OpsRequestAttributes{
		ReasoningEffort: "high", ServiceTier: "priority",
		ImageSize: "2K", ImageInputSize: "1536x1024", ImageResponseFormat: "url", ImageCount: 3,
	}, GetOpsRequestAttributes(c))

	// 不在 usage_logs CHECK 约束里的返回格式不记；nil 只清对应字段。
	SetOpsImageResponseFormat(c, "png")
	SetOpsReasoningEffort(c, nil)
	got := GetOpsRequestAttributes(c)
	require.Empty(t, got.ImageResponseFormat)
	require.Empty(t, got.ReasoningEffort)
	require.Equal(t, "priority", got.ServiceTier)

	// 客户端原样传入的超长尺寸、越界张数落 NULL，不截断、不拖垮整批写入。
	bounded := OpsRequestAttributes{
		ReasoningEffort: "high", ImageInputSize: strings.Repeat("9", 33), ImageCount: math.MaxInt32 + 1,
	}.boundForStorage()
	require.Equal(t, OpsRequestAttributes{ReasoningEffort: "high"}, bounded)

	c.Set(OpsRequestStartedAtKey, time.Now().Add(-1500*time.Millisecond))
	elapsed := OpsRequestElapsedMs(c)
	require.NotNil(t, elapsed)
	require.GreaterOrEqual(t, *elapsed, 1500)

	// 换号：按尝试计算的属性清空，请求侧图片参数保留。
	SetOpsImageResponseFormat(c, "url")
	ClearOpsAttemptRequestAttributes(c)
	require.Equal(t, OpsRequestAttributes{ImageSize: "2K", ImageInputSize: "1536x1024", ImageCount: 3}, GetOpsRequestAttributes(c))

	// WebSocket 新 turn：属性清空、耗时从该 turn 起算。
	BeginOpsStreamTurn(c, 2)
	require.Equal(t, OpsRequestAttributes{}, GetOpsRequestAttributes(c))
	require.Less(t, *OpsRequestElapsedMs(c), 1000)
}

// OpenAI /v1/responses：失败时登记的推理强度 / service tier 必须等于同一请求成功时
// 转发结果里的值（usage_logs.reasoning_effort / service_tier 直接取自转发结果）。
func TestOpenAIResponsesForwardFailureRecordsSameAttributesAsSuccess(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantEffort string
		wantTier   string
	}{
		{
			name:       "effort derived from model suffix",
			body:       `{"model":"gpt-5.4-high","stream":false,"instructions":"x","input":"hello"}`,
			wantEffort: "high",
		},
		{
			name:       "explicit effort and tier",
			body:       `{"model":"gpt-5.4","stream":false,"reasoning":{"effort":"x-high"},"service_tier":"fast","instructions":"x","input":"hello"}`,
			wantEffort: "xhigh",
			wantTier:   "priority",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`)),
				},
				{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
				},
			}}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			account := &Account{
				ID: 31, Name: "apikey-a", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://upstream.example.com"},
				Status:      StatusActive, Schedulable: true,
			}

			failCtx := newOpsAttrTestContext("/v1/responses", body)
			_, err := svc.Forward(context.Background(), failCtx, account, body)
			require.Error(t, err)
			failed := GetOpsRequestAttributes(failCtx)

			okCtx := newOpsAttrTestContext("/v1/responses", body)
			result, err := svc.Forward(context.Background(), okCtx, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)

			require.Equal(t, tc.wantEffort, failed.ReasoningEffort)
			require.Equal(t, optionalStringValue(result.ReasoningEffort), failed.ReasoningEffort)
			require.Equal(t, tc.wantTier, failed.ServiceTier)
			require.Equal(t, optionalStringValue(result.ServiceTier), failed.ServiceTier)
		})
	}
}

// Anthropic /v1/messages：speed=fast 的档位在转发失败时同样登记（成功时由同一 defer 写进结果）。
func TestGatewayForwardFailureRecordsAnthropicFastServiceTier(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","speed":"fast","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	parse := func() *ParsedRequest {
		parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
		require.NoError(t, err)
		return parsed
	}
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: http.StatusInternalServerError, body: `{"type":"error","error":{"type":"api_error","message":"boom"}}`},
		{status: http.StatusOK, body: errorHandlingRuleSuccessBody},
	}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{})

	failCtx := newErrorHandlingRuleTestContext()
	result, err := svc.Forward(context.Background(), failCtx, newErrorHandlingRuleForwardAccount(), parse())
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, "fast", GetOpsRequestAttributes(failCtx).ServiceTier)

	okCtx := newErrorHandlingRuleTestContext()
	result, err = svc.Forward(context.Background(), okCtx, newErrorHandlingRuleForwardAccount(), parse())
	require.NoError(t, err)
	require.Equal(t, "fast", optionalStringValue(result.ServiceTier))
}

// Gemini 原生 generateContent：失败时的推理强度与图片参数等于成功时转发结果里的值。
func TestGeminiForwardNativeFailureRecordsSameAttributesAsSuccess(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		body      string
		want      OpsRequestAttributes
		wantImage bool
	}{
		{
			name:  "thinking level",
			model: "gemini-2.5-flash",
			body:  `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}`,
			want:  OpsRequestAttributes{ReasoningEffort: "high"},
		},
		{
			name:      "image model",
			model:     "gemini-3.1-flash-image",
			body:      `{"contents":[{"role":"user","parts":[{"text":"draw"}]}],"generationConfig":{"imageConfig":{"imageSize":"4K"}}}`,
			want:      OpsRequestAttributes{ImageSize: "4K", ImageInputSize: "4K", ImageCount: 1},
			wantImage: true,
		},
	}
	account := &Account{ID: 710, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			path := "/v1beta/models/" + tc.model + ":generateContent"

			failSvc := &GeminiMessagesCompatService{
				httpUpstream: &geminiCompatHTTPUpstreamStub{err: errors.New("unexpected EOF")},
				cfg:          &config.Config{},
			}
			failCtx := newOpsAttrTestContext(path, body)
			_, err := failSvc.ForwardNative(context.Background(), failCtx, account, tc.model, "generateContent", false, body)
			require.Error(t, err)
			failed := GetOpsRequestAttributes(failCtx)
			require.Equal(t, tc.want, failed)

			okSvc := &GeminiMessagesCompatService{
				httpUpstream: &geminiCompatHTTPUpstreamStub{response: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)),
				}},
				cfg: &config.Config{},
			}
			okCtx := newOpsAttrTestContext(path, body)
			result, err := okSvc.ForwardNative(context.Background(), okCtx, account, tc.model, "generateContent", false, body)
			require.NoError(t, err)
			require.Equal(t, optionalStringValue(result.ReasoningEffort), failed.ReasoningEffort)
			if tc.wantImage {
				require.Equal(t, result.ImageSize, failed.ImageSize)
				require.Equal(t, result.ImageInputSize, failed.ImageInputSize)
				require.Equal(t, result.ImageCount, failed.ImageCount)
			}
		})
	}
}
