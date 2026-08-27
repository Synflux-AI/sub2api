//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 2026-08-26 事故：账号 60 的 /v1/images/edits 在同一秒吃到 128 条
// `http2: client connection lost`，全部以 502 直吐给客户端 —— 一次重试、一次换号都没有。
// 根因是 images 转发在传输层失败时返回裸 error，handler 的 errors.As(*UpstreamFailoverError)
// 因此不成立，整个 failover 块被跳过。
//
// 这三个测试锁住修复：传输层错误必须返回 *UpstreamFailoverError（502），且只记一条
// ops 上游错误事件（原先本地记一条、helper 再记一条会污染 ops_error_logs.upstream_errors，
// 而这张表正是定案「128 条」的依据）。

func newImagesTransportTestService(upstream *failingOpenAIHTTPUpstream) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		accountRepo:  &openaiTransportAccountRepoStub{},
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
}

func newImagesTransportTestContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c, rec
}

func opsUpstreamErrorEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	value, ok := c.Get(OpsUpstreamErrorsKey)
	if !ok {
		return nil
	}
	events, ok := value.([]*OpsUpstreamErrorEvent)
	require.True(t, ok, "ops upstream errors must be []*OpsUpstreamErrorEvent")
	return events
}

func TestForwardOpenAIImagesAPIKey_TransportErrorReturnsFailover(t *testing.T) {
	upstream := &failingOpenAIHTTPUpstream{
		err: errors.New(`Post "https://subdirect.aicodexvip.top/v1/images/edits": http2: client connection lost`),
	}
	svc := newImagesTransportTestService(upstream)
	account := &Account{
		ID:          60,
		Name:        "aicodexvip-img2",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://subdirect.aicodexvip.top/v1"},
	}
	c, rec := newImagesTransportTestContext("/v1/images/edits")
	parsed := &OpenAIImagesRequest{
		Endpoint:    openAIImagesEditsEndpoint,
		ContentType: "application/json",
		Model:       "gpt-image-1",
		N:           1,
	}

	_, err := svc.forwardOpenAIImagesAPIKey(context.Background(), c, account,
		[]byte(`{"model":"gpt-image-1","prompt":"a cat"}`), parsed, "")

	require.Equal(t, 1, upstream.calls)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"images transport error must return *UpstreamFailoverError so the handler fails over")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, 0, rec.Body.Len(), "service must not write the response: the handler owns it")
}

func TestForwardOpenAIImagesAPIKey_TransportErrorRecordsSingleOpsEvent(t *testing.T) {
	upstream := &failingOpenAIHTTPUpstream{
		err: errors.New(`Post "https://subdirect.aicodexvip.top/v1/images/edits": http2: client connection lost`),
	}
	svc := newImagesTransportTestService(upstream)
	account := &Account{
		ID:          60,
		Name:        "aicodexvip-img2",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://subdirect.aicodexvip.top/v1"},
	}
	c, _ := newImagesTransportTestContext("/v1/images/edits")
	parsed := &OpenAIImagesRequest{
		Endpoint:    openAIImagesEditsEndpoint,
		ContentType: "application/json",
		Model:       "gpt-image-1",
		N:           1,
	}

	_, _ = svc.forwardOpenAIImagesAPIKey(context.Background(), c, account,
		[]byte(`{"model":"gpt-image-1","prompt":"a cat"}`), parsed, "")

	events := opsUpstreamErrorEvents(t, c)
	require.Len(t, events, 1, "one transport failure must produce exactly one upstream error event")
	require.Equal(t, 0, events[0].UpstreamStatusCode)
	require.Equal(t, "request_error", events[0].Kind)
	require.Equal(t, PlatformOpenAI, events[0].Platform)
	require.Equal(t, int64(60), events[0].AccountID)
	require.NotEmpty(t, events[0].UpstreamURL, "upstream URL must survive the move to the shared helper")
	require.Contains(t, events[0].Message, "client connection lost")
}

func TestForwardOpenAIImagesOAuth_TransportErrorReturnsFailover(t *testing.T) {
	upstream := &failingOpenAIHTTPUpstream{
		err: errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": http2: client connection lost`),
	}
	svc := newImagesTransportTestService(upstream)
	account := &Account{
		ID:          61,
		Name:        "codex-oauth-img",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "at-test", "expires_at": float64(1 << 40)},
	}
	c, rec := newImagesTransportTestContext("/v1/images/generations")
	parsed := &OpenAIImagesRequest{
		Endpoint:    openAIImagesGenerationsEndpoint,
		ContentType: "application/json",
		Model:       "gpt-image-1",
		Prompt:      "a cat",
		N:           1,
	}

	_, err := svc.forwardOpenAIImagesOAuth(context.Background(), c, account, parsed, "")

	require.Equal(t, 1, upstream.calls)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"images OAuth transport error must return *UpstreamFailoverError so the handler fails over")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, 0, rec.Body.Len())

	events := opsUpstreamErrorEvents(t, c)
	require.Len(t, events, 1, "one transport failure must produce exactly one upstream error event")
	require.NotEmpty(t, events[0].UpstreamURL)
}
