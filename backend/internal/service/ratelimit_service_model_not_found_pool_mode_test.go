//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// #274：池模式账号的上游本身是个池，一次 model-not-found 可能只是池内暂时状态，
// 不能像官方/OAuth 账号那样写 30 分钟 per-model 冷却。

type poolModeModelNotFoundCase struct {
	name       string
	statusCode int
	body       []byte
	model      string
	reason     string
}

func poolModeModelNotFoundCases() []poolModeModelNotFoundCase {
	return []poolModeModelNotFoundCase{
		{
			name:       "404 model_not_found",
			statusCode: http.StatusNotFound,
			body:       []byte(`{"error":{"code":"model_not_found","message":"Model not found"}}`),
			model:      "gpt-5.4",
			reason:     upstreamModelNotFoundReason,
		},
		{
			name:       "401 unknown model",
			statusCode: http.StatusUnauthorized,
			body:       []byte(`{"error":{"message":"unknown model definitely-not-real"}}`),
			model:      "definitely-not-real",
			reason:     upstreamModelNotFound401Reason,
		},
	}
}

func poolModeModelNotFoundAccount() *Account {
	account := openAIModelNotFoundTempAccount()
	account.Credentials["pool_mode"] = true
	return account
}

func TestOpenAIModelNotFound_PoolModeSkipsModelCooldown(t *testing.T) {
	for _, tc := range poolModeModelNotFoundCases() {
		t.Run(tc.name, func(t *testing.T) {
			repo := &modelNotFoundAccountRepoStub{}
			svc := &OpenAIGatewayService{
				rateLimitService: &RateLimitService{accountRepo: repo},
			}
			account := poolModeModelNotFoundAccount()

			shouldDisable := svc.handleOpenAIAccountUpstreamError(
				context.Background(), account, tc.statusCode, http.Header{}, tc.body, tc.model,
			)

			// 返回值决定本次请求换号，保持与改动前一致；只是不再写冷却。
			require.True(t, shouldDisable)
			require.Empty(t, repo.modelRateLimitCalls)
			require.Zero(t, repo.tempCalls)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, tc.model))
			require.True(t, account.IsSchedulableForModelWithContext(context.Background(), tc.model))
		})
	}
}

func TestOpenAIModelNotFound_PoolModeWithCustomErrorCodesKeepsModelCooldown(t *testing.T) {
	for _, tc := range poolModeModelNotFoundCases() {
		t.Run(tc.name, func(t *testing.T) {
			repo := &modelNotFoundAccountRepoStub{}
			svc := &OpenAIGatewayService{
				rateLimitService: &RateLimitService{accountRepo: repo},
			}
			account := poolModeModelNotFoundAccount()
			account.Credentials["custom_error_codes_enabled"] = true
			account.Credentials["custom_error_codes"] = []any{float64(http.StatusNotFound), float64(http.StatusUnauthorized)}

			shouldDisable := svc.handleOpenAIAccountUpstreamError(
				context.Background(), account, tc.statusCode, http.Header{}, tc.body, tc.model,
			)

			require.True(t, shouldDisable)
			require.Len(t, repo.modelRateLimitCalls, 1)
			require.Equal(t, tc.model, repo.modelRateLimitCalls[0].scope)
			require.Equal(t, tc.reason, repo.modelRateLimitCalls[0].reason)
		})
	}
}

func TestOpenAIModelNotFound_NonPoolModeKeepsModelCooldown(t *testing.T) {
	for _, tc := range poolModeModelNotFoundCases() {
		t.Run(tc.name, func(t *testing.T) {
			repo := &modelNotFoundAccountRepoStub{}
			svc := &OpenAIGatewayService{
				rateLimitService: &RateLimitService{accountRepo: repo},
			}
			account := openAIModelNotFoundTempAccount()

			shouldDisable := svc.handleOpenAIAccountUpstreamError(
				context.Background(), account, tc.statusCode, http.Header{}, tc.body, tc.model,
			)

			require.True(t, shouldDisable)
			require.Len(t, repo.modelRateLimitCalls, 1)
			require.Equal(t, tc.model, repo.modelRateLimitCalls[0].scope)
			require.Equal(t, tc.reason, repo.modelRateLimitCalls[0].reason)
		})
	}
}

func TestRateLimitService_HandleUpstreamModelNotFound_PoolModeSkipsCooldown(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}

	handled := svc.HandleUpstreamModelNotFound(
		context.Background(),
		poolModeModelNotFoundAccount(),
		"gpt-5.4",
		http.StatusNotFound,
		[]byte(`{"error":{"code":"model_not_found","message":"Model not found"}}`),
	)

	require.True(t, handled)
	require.Empty(t, repo.modelRateLimitCalls)
}

// 回归：404 不在 shouldFailover 名单里，本次换号由 handleErrorResponse 依据
// shouldDisable 决定。池模式即使把 404 配成同号重试码，结论仍是换号、不同号重试。
func TestHandleErrorResponse_PoolModeModelNotFoundStillFailsOverWithoutCooldown(t *testing.T) {
	c, rec := newOpenAIUpstreamErrorTestContext(t)
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		cfg:              &config.Config{},
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := poolModeModelNotFoundAccount()
	account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusNotFound)}
	body := `{"error":{"code":"model_not_found","message":"Model not found"}}`

	_, err := svc.handleErrorResponse(
		context.Background(),
		newOpenAIUpstreamErrorResponse(http.StatusNotFound, body),
		c, account, nil, "gpt-5.4",
	)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusNotFound, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, IsResponseCommitted(c))
	require.Zero(t, rec.Body.Len())
	require.Empty(t, repo.modelRateLimitCalls)
}

// 回归：401 走 failoverOpenAIUpstreamHTTPError，同号重试由
// !shouldDisable && IsPoolModeRetryableStatus 决定，结论保持不变。
func TestFailoverOpenAIUpstreamHTTPError_PoolModeModel401KeepsRetryDecisionWithoutCooldown(t *testing.T) {
	c, _ := newOpenAIUpstreamErrorTestContext(t)
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		cfg:              &config.Config{},
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := poolModeModelNotFoundAccount()
	account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusUnauthorized)}
	body := []byte(`{"error":{"message":"unknown model definitely-not-real"}}`)

	failoverErr := svc.failoverOpenAIUpstreamHTTPError(
		context.Background(), c, account,
		newOpenAIUpstreamErrorResponse(http.StatusUnauthorized, string(body)),
		body, "unknown model definitely-not-real", "definitely-not-real",
	)

	require.NotNil(t, failoverErr)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Empty(t, repo.modelRateLimitCalls)
}
