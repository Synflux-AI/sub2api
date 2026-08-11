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
	apiKeyAccount := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	oauthAccount := &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	openAIAPIKeyAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	bedrockAccount := &Account{Platform: PlatformAnthropic, Type: AccountTypeBedrock}
	rule := ErrorHandlingRule{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}

	t.Run("enabled API Key", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true, Rules: []ErrorHandlingRule{rule}})
		require.True(t, svc.errorHandlingRulesActive(context.Background(), apiKeyAccount))
	})
	t.Run("non API Key", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true, Rules: []ErrorHandlingRule{rule}})
		require.False(t, svc.errorHandlingRulesActive(context.Background(), oauthAccount))
	})
	t.Run("other platform API Key", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true, Rules: []ErrorHandlingRule{rule}})
		require.False(t, svc.errorHandlingRulesActive(context.Background(), openAIAPIKeyAccount))
	})
	t.Run("Anthropic Bedrock", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{Enabled: true, Rules: []ErrorHandlingRule{rule}})
		require.False(t, svc.errorHandlingRulesActive(context.Background(), bedrockAccount))
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
	rule, _ := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}, 500, []byte(`{}`))
	require.Nil(t, rule)
}

func TestMatchErrorHandlingRuleForAccountResolvesRetryLimit(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{
			Enabled: true, DefaultRetryCount: 3,
			Rules: []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}},
		})
		rule, limit := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}, 500, []byte(`{}`))
		require.NotNil(t, rule)
		require.Equal(t, 3, limit)
	})
	t.Run("override", func(t *testing.T) {
		svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{
			Enabled: true, DefaultRetryCount: 3,
			Rules: []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(0)}},
		})
		rule, limit := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}, 500, []byte(`{}`))
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
	rule, _ := svc.matchErrorHandlingRuleForAccount(context.Background(), &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}, 400, respBody)
	require.NotNil(t, rule)
	restoreErrorHandlingRuleBody(resp, respBody)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, respBody, got)
}

func TestErrorHandlingRuleFailoverMetadataIsScopedToStreamRules(t *testing.T) {
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`)
	decision := errorHandlingRuleDecision{
		Matched: true, RuleID: "r1", ExhaustedAction: ErrorHandlingExhaustedActionPassthrough,
	}

	t.Run("HTTP rule preserves legacy pool retry metadata", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: http.NoBody}
		err := (&GatewayService{}).errorHandlingRuleFailover(context.Background(), resp, body, account, "claude", decision, false)
		failoverErr, ok := err.(*UpstreamFailoverError)
		require.True(t, ok)
		require.True(t, failoverErr.RetryableOnSameAccount)
		require.Equal(t, NextAccountLegacyRetry, failoverErr.NextAccountAction)

		restored, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		require.Equal(t, body, restored, "failover side effects must receive the virtual response body")
	})

	t.Run("stream rule owns retry budget", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: http.NoBody}
		err := (&GatewayService{}).errorHandlingRuleFailover(context.Background(), resp, body, account, "claude", decision, true)
		failoverErr, ok := err.(*UpstreamFailoverError)
		require.True(t, ok)
		require.False(t, failoverErr.RetryableOnSameAccount)
		require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	})
}
