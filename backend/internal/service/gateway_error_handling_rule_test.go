package service

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestErrorHandlingRuleTrackerConsume(t *testing.T) {
	var tracker errorHandlingRuleTracker
	require.True(t, tracker.consume("r1", 2))
	require.True(t, tracker.consume("r1", 2))
	require.False(t, tracker.consume("r1", 2))
	require.True(t, tracker.consume("r2", 1))
	require.False(t, tracker.consume("r2", 1))
}

func TestErrorHandlingRuleTrackerZeroBudget(t *testing.T) {
	var tracker errorHandlingRuleTracker
	require.False(t, tracker.consume("r1", 0))
}

func TestErrorHandlingRuleTrackerKeepsIndependentStreamBudgetsByRule(t *testing.T) {
	var tracker errorHandlingRuleTracker
	require.True(t, tracker.consumeForRule("r1", 1))
	require.True(t, tracker.consumeForRule("r2", 1))
	require.False(t, tracker.consumeForRule("r1", 1))
	require.False(t, tracker.consumeForRule("r2", 1))
}

func newErrorHandlingRuleService(t *testing.T, settings *ErrorHandlingRuleSettings) *GatewayService {
	t.Helper()
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := &GatewayService{settingService: NewSettingService(repo, &config.Config{})}
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), settings))
	return svc
}

func TestErrorHandlingRulesActive(t *testing.T) {
	apiKeyAccount := &Account{Type: AccountTypeAPIKey}
	oauthAccount := &Account{Type: AccountTypeOAuth}
	rule := ErrorHandlingRule{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}

	t.Run("enabled API Key", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true, Rules: []ErrorHandlingRule{rule}})
		require.True(t, svc.errorHandlingRulesActive(context.Background(), apiKeyAccount))
	})
	t.Run("non API Key", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true, Rules: []ErrorHandlingRule{rule}})
		require.False(t, svc.errorHandlingRulesActive(context.Background(), oauthAccount))
	})
	t.Run("disabled", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: false, Rules: []ErrorHandlingRule{rule}})
		require.False(t, svc.errorHandlingRulesActive(context.Background(), apiKeyAccount))
	})
	t.Run("empty rules", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true})
		require.False(t, svc.errorHandlingRulesActive(context.Background(), apiKeyAccount))
	})
	t.Run("nil account", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true, Rules: []ErrorHandlingRule{rule}})
		require.False(t, svc.errorHandlingRulesActive(context.Background(), nil))
	})
}

func TestMatchErrorHandlingRuleForAccountReturnsNilWhenInactive(t *testing.T) {
	svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{
		Enabled: false, Rules: []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}},
	})
	rule, _ := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Type: AccountTypeAPIKey}, 500, []byte(`{}`))
	require.Nil(t, rule)
}

func TestMatchErrorHandlingRuleForAccountResolvesRetryLimit(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{
			Enabled: true, DefaultRetryCount: 3,
			Rules: []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}},
		})
		rule, limit := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Type: AccountTypeAPIKey}, 500, []byte(`{}`))
		require.NotNil(t, rule)
		require.Equal(t, 3, limit)
	})
	t.Run("override", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{
			Enabled: true, DefaultRetryCount: 3,
			Rules: []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(0)}},
		})
		rule, limit := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Type: AccountTypeAPIKey}, 500, []byte(`{}`))
		require.NotNil(t, rule)
		require.Equal(t, 0, limit)
	})
}

func TestApplyErrorHandlingRulePassthroughRestoresBodyForErrorResponse(t *testing.T) {
	svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", Keywords: []string{"prompt is too long"}, Action: ErrorHandlingActionPassthrough}},
	})
	respBody := []byte(`{"error":{"message":"prompt is too long"}}`)
	resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: http.NoBody}
	rule, _ := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Type: AccountTypeAPIKey}, 400, respBody)
	require.NotNil(t, rule)
	restoreErrorHandlingRuleBody(resp, respBody)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, respBody, got)
}
