package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShouldFailoverOpenAIPassthroughResponseAPIKeyStatuses(t *testing.T) {
	apiKeyAccount := &Account{Type: AccountTypeAPIKey}
	oauthAccount := &Account{Type: AccountTypeOAuth}
	genericBody := []byte(`{"error":{"message":"upstream error"}}`)

	t.Run("api key 400/422 trigger failover", func(t *testing.T) {
		require.True(t, shouldFailoverOpenAIPassthroughResponse(apiKeyAccount, http.StatusBadRequest, genericBody))
		require.True(t, shouldFailoverOpenAIPassthroughResponse(apiKeyAccount, http.StatusUnprocessableEntity, genericBody))
	})

	t.Run("api key 5xx family still triggers failover", func(t *testing.T) {
		for _, status := range []int{
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
			520, 521, 522, 523, 524,
		} {
			require.True(t, shouldFailoverOpenAIPassthroughResponse(apiKeyAccount, status, genericBody), "status %d", status)
		}
	})

	t.Run("oauth 400/422/5xx do not trigger failover", func(t *testing.T) {
		for _, status := range []int{
			http.StatusBadRequest,
			http.StatusUnprocessableEntity,
			http.StatusInternalServerError,
		} {
			require.False(t, shouldFailoverOpenAIPassthroughResponse(oauthAccount, status, genericBody), "status %d", status)
		}
	})

	t.Run("context window 400 never fails over", func(t *testing.T) {
		contextWindowBody := []byte(`{"error":{"message":"Your input exceeds the context window of this model"}}`)
		require.False(t, shouldFailoverOpenAIPassthroughResponse(apiKeyAccount, http.StatusBadRequest, contextWindowBody))
	})

	t.Run("429 and 529 fail over for all account types", func(t *testing.T) {
		require.True(t, shouldFailoverOpenAIPassthroughResponse(oauthAccount, http.StatusTooManyRequests, genericBody))
		require.True(t, shouldFailoverOpenAIPassthroughResponse(apiKeyAccount, 529, genericBody))
	})

	t.Run("other client errors do not fail over", func(t *testing.T) {
		require.False(t, shouldFailoverOpenAIPassthroughResponse(apiKeyAccount, http.StatusNotFound, genericBody))
		require.False(t, shouldFailoverOpenAIPassthroughResponse(apiKeyAccount, http.StatusConflict, genericBody))
	})
}
