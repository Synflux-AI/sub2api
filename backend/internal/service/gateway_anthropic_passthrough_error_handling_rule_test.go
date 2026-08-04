package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newErrorHandlingRulePassthroughAccount() *Account {
	return &Account{
		ID: 702, Name: "apikey-passthrough-error-rule-test", Platform: PlatformAnthropic,
		Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "upstream-anthropic-key", "base_url": "https://api.anthropic.com"},
		Extra:       map[string]any{"anthropic_passthrough": true}, Status: StatusActive, Schedulable: true,
	}
}

func newErrorHandlingRulePassthroughService(t *testing.T, upstream *sequencedHTTPUpstream, ruleSettings *ErrorHandlingRuleSettings) *GatewayService {
	t.Helper()
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream,
		settingService: NewSettingService(repo, cfg), rateLimitService: &RateLimitService{}, deferredService: &DeferredService{},
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), ruleSettings))
	return svc
}

func TestPassthroughErrorHandlingRuleRetriesInPlaceThenSucceeds(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 422, body: `{"error":{"message":"Failed to deserialize request body"}}`},
		{status: 200, body: errorHandlingRuleSuccessBody},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{422}, Keywords: []string{"Failed to deserialize"}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)}},
	})
	result, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleTestParsed(t))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.calls)
}

// 直传路径上引擎跑在内置签名换号（forwardAnthropicAPIKeyPassthroughWithInput 末尾的
// shouldFailoverSignatureError）之前，靠 applyErrorHandlingRule 里的让位判断维持
// issue #122 决策 5 的优先级。这条用例同时锁住该路径把 RequestModel 传了进去——
// 传空模型名会让让位判断永远不成立。
func TestPassthroughBuiltinSignatureFailoverPreemptsErrorHandlingRule(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 400, body: `{"error":{"message":"Invalid signature in thinking block"}}`},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{400}, Action: ErrorHandlingActionPassthrough}},
	})
	require.NoError(t, svc.settingService.SetRectifierSettings(context.Background(), &RectifierSettings{
		Enabled: true, APIKeySignatureFailoverEnabled: true,
	}))

	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "签名错误应由内置逻辑换号，而不是被规则 passthrough 掉")
	require.False(t, IsResponseCommitted(c))
}

func TestPassthroughErrorHandlingRuleFailsOverWhenRetryCountZero(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 422, body: `{"error":{"message":"Failed to deserialize request body"}}`}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{422}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(0)}},
	})
	_, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 422, failoverErr.StatusCode)
	require.Equal(t, 1, upstream.calls)
}
