//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAINonStreamingBodyReadErrorCloser struct {
	payload []byte
	err     error
}

func (r *openAINonStreamingBodyReadErrorCloser) Read(p []byte) (int, error) {
	if len(r.payload) > 0 {
		n := copy(p, r.payload)
		r.payload = r.payload[n:]
		return n, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

func (r *openAINonStreamingBodyReadErrorCloser) Close() error { return nil }

func newOpenAINonStreamingBodyReadTestContext(path string, requestCtx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if requestCtx != nil {
		req = req.WithContext(requestCtx)
	}
	c.Request = req
	return c, rec
}

func newOpenAINonStreamingInterruptedResponse(err error) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: &openAINonStreamingBodyReadErrorCloser{
			payload: []byte(`{"id":"partial"`),
			err:     err,
		},
	}
}

func requireOpenAINonStreamingBodyReadFailover(
	t *testing.T,
	c *gin.Context,
	err error,
	account *Account,
	passthrough bool,
	upstreamURL string,
) {
	t.Helper()
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, c.Writer.Written(), "service must leave the response uncommitted for handler failover")

	events := opsUpstreamErrorEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "request_error", events[0].Kind)
	require.Equal(t, 0, events[0].UpstreamStatusCode)
	require.Equal(t, account.Platform, events[0].Platform)
	require.Equal(t, account.ID, events[0].AccountID)
	require.Equal(t, passthrough, events[0].Passthrough)
	require.Equal(t, upstreamURL, events[0].UpstreamURL)
	require.Contains(t, events[0].Message, "client connection lost")
}

func TestOpenAINonStreamingBodyReadErrorReturnsFailoverAndRecordsOps(t *testing.T) {
	readErr := errors.New("http2: client connection lost")
	tests := []struct {
		name        string
		path        string
		account     *Account
		passthrough bool
		upstreamURL string
		invoke      func(*OpenAIGatewayService, context.Context, *gin.Context, *Account, *http.Response) error
	}{
		{
			name:        "responses_passthrough",
			path:        "/v1/responses",
			account:     &Account{ID: 81, Name: "passthrough", Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			passthrough: true,
			invoke: func(s *OpenAIGatewayService, ctx context.Context, c *gin.Context, account *Account, resp *http.Response) error {
				_, err := s.handleNonStreamingResponsePassthrough(ctx, resp, c, account, "gpt-5.6", "gpt-5.6")
				return err
			},
		},
		{
			name:    "responses_transformed",
			path:    "/v1/responses",
			account: &Account{ID: 82, Name: "oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			invoke: func(s *OpenAIGatewayService, ctx context.Context, c *gin.Context, account *Account, resp *http.Response) error {
				_, err := s.handleNonStreamingResponse(ctx, resp, c, account, "gpt-5.6", "gpt-5.6")
				return err
			},
		},
		{
			name:        "native_anthropic",
			path:        "/v1/messages",
			account:     &Account{ID: 83, Name: "kimi-native", Platform: PlatformKimi, Type: AccountTypeAPIKey},
			passthrough: true,
			invoke: func(s *OpenAIGatewayService, ctx context.Context, c *gin.Context, account *Account, resp *http.Response) error {
				_, err := s.handleNativeAnthropicBufferedResponse(
					ctx, resp, c, account, "kimi-k2", "kimi-k2", "kimi-k2", nil, time.Now())
				return err
			},
		},
		{
			name:        "images_api_key",
			path:        "/v1/images/generations",
			account:     &Account{ID: 84, Name: "image", Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			upstreamURL: "https://api.example.test/v1/images/generations",
			invoke: func(s *OpenAIGatewayService, ctx context.Context, c *gin.Context, account *Account, resp *http.Response) error {
				_, _, _, _, err := s.handleOpenAIImagesNonStreamingResponse(
					ctx, resp, c, account, "https://api.example.test/v1/images/generations")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c, _ := newOpenAINonStreamingBodyReadTestContext(tt.path, ctx)
			svc := &OpenAIGatewayService{cfg: &config.Config{}}

			err := tt.invoke(svc, ctx, c, tt.account, newOpenAINonStreamingInterruptedResponse(readErr))

			requireOpenAINonStreamingBodyReadFailover(
				t, c, err, tt.account, tt.passthrough, tt.upstreamURL)
		})
	}
}

func TestOpenAINonStreamingBodyReadErrorExclusionsDoNotFailover(t *testing.T) {
	t.Run("response_too_large", func(t *testing.T) {
		ctx := context.Background()
		c, rec := newOpenAINonStreamingBodyReadTestContext("/v1/responses", ctx)
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		svc.cfg.Gateway.UpstreamResponseReadMaxBytes = 3
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("toolong")),
		}
		account := &Account{ID: 91, Name: "too-large", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

		_, err := svc.handleNonStreamingResponsePassthrough(ctx, resp, c, account, "gpt-5.6", "gpt-5.6")

		require.ErrorIs(t, err, ErrUpstreamResponseBodyTooLarge)
		var failoverErr *UpstreamFailoverError
		require.NotErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Empty(t, opsUpstreamErrorEvents(t, c))
	})

	tests := []struct {
		name          string
		readErr       error
		cancelRequest bool
	}{
		{name: "context_canceled", readErr: context.Canceled},
		{name: "context_deadline_exceeded", readErr: context.DeadlineExceeded},
		{name: "request_context_canceled", readErr: io.ErrUnexpectedEOF, cancelRequest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCtx := context.Background()
			if tt.cancelRequest {
				canceledCtx, cancel := context.WithCancel(requestCtx)
				cancel()
				requestCtx = canceledCtx
			}
			c, rec := newOpenAINonStreamingBodyReadTestContext("/v1/responses", requestCtx)
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 92, Name: "canceled", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

			_, err := svc.handleNonStreamingResponsePassthrough(
				context.Background(), newOpenAINonStreamingInterruptedResponse(tt.readErr), c, account, "gpt-5.6", "gpt-5.6")

			require.ErrorIs(t, err, tt.readErr)
			var failoverErr *UpstreamFailoverError
			require.NotErrorAs(t, err, &failoverErr)
			require.Zero(t, rec.Body.Len())
			require.Empty(t, opsUpstreamErrorEvents(t, c))
		})
	}
}
