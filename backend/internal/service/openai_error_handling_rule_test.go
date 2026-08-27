//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// #189：错误处理规则接入 OpenAI 侧。
//
// 引擎原先 11 个（实测 8 个）调用点全在 *GatewayService 上，OpenAI 走独立类型
// *OpenAIGatewayService，调用点 0 —— 所以「只加一个平台勾选框」是空壳。这里补的是
// 真正的执行层：把平台中立的决策映射到既有的 UpstreamFailoverError 字段。

func newOpenAIRuleSettingService(t *testing.T, rules ...ErrorHandlingRule) *SettingService {
	t.Helper()
	payload, err := json.Marshal(ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1, Rules: rules,
	})
	require.NoError(t, err)
	repo := &gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyErrorHandlingRules: string(payload),
	}}
	return NewSettingService(repo, &config.Config{})
}

func newOpenAIRuleService(t *testing.T, upstream *failingOpenAIHTTPUpstream, rules ...ErrorHandlingRule) *OpenAIGatewayService {
	t.Helper()
	return &OpenAIGatewayService{
		accountRepo:    &openaiTransportAccountRepoStub{},
		httpUpstream:   upstream,
		settingService: newOpenAIRuleSettingService(t, rules...),
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
}

func openAIRuleAccount() *Account {
	return &Account{
		ID: 60, Name: "aicodexvip-img2", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
	}
}

var openAILostPingErr = errors.New(`Post "https://subdirect.aicodexvip.top/v1/images/edits": http2: client connection lost`)

// 事故复刻：lost-ping 是传输层错误，没有 HTTP 状态码（那 128 条的
// upstream_status_code 在库里是 NULL）。合成 502 + OpenAI 形状错误体后喂给引擎，
// 规则才有东西可匹配。
func TestOpenAIErrorHandlingRule_TransportErrorMatchesSynthetic502(t *testing.T) {
	svc := newOpenAIRuleService(t, nil, ErrorHandlingRule{
		ID: "images-lost-ping", Name: "images 连接丢失换号",
		StatusCodes: []int{502}, Keywords: []string{"connection lost"},
		Action: ErrorHandlingActionFailover, Platforms: []string{PlatformOpenAI},
	})
	c, _ := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, openAIRuleAccount(), openAILostPingErr, false)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, "images-lost-ping", failoverErr.ErrorRuleID)
	require.False(t, failoverErr.RetryableOnSameAccount, "failover 动作是换号，不是同号重试")

	// upstream_errors 里出现 error_handling_rule_* 是判断「引擎有没有被绕过」的唯一抓手。
	events := opsUpstreamErrorEvents(t, c)
	require.NotEmpty(t, events)
	require.Equal(t, "error_handling_rule_failover", events[len(events)-1].Kind)
}

func TestOpenAIErrorHandlingRule_RetryActionSetsSameAccountBudget(t *testing.T) {
	retry := 2
	svc := newOpenAIRuleService(t, nil, ErrorHandlingRule{
		ID: "retry-rule", StatusCodes: []int{502}, Keywords: []string{"connection lost"},
		Action: ErrorHandlingActionRetry, RetryCount: &retry, Platforms: []string{PlatformOpenAI},
	})
	c, _ := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, openAIRuleAccount(), openAILostPingErr, false)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.NotNil(t, failoverErr.RuleRetryLimit, "规则驱动的重试预算必须显式带出来，"+
		"否则非 pool-mode 账号的 effectiveSameAccountRetryLimit 是 0，retry 会静默退化成换号")
	require.Equal(t, 2, *failoverErr.RuleRetryLimit)
}

func TestOpenAIErrorHandlingRule_PassthroughStopsAccountRotation(t *testing.T) {
	svc := newOpenAIRuleService(t, nil, ErrorHandlingRule{
		ID: "passthrough-rule", StatusCodes: []int{502}, Keywords: []string{"connection lost"},
		Action: ErrorHandlingActionPassthrough, Platforms: []string{PlatformOpenAI},
	})
	c, _ := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, openAIRuleAccount(), openAILostPingErr, false)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.False(t, failoverErr.ShouldRetryNextAccount(), "passthrough 必须立刻停止换号")
	require.Equal(t, ErrorHandlingExhaustedActionPassthrough, failoverErr.ExhaustedAction)
	require.NotEmpty(t, failoverErr.SafeErrorType)
	require.NotEmpty(t, failoverErr.SafeErrorMessage)
}

// 平台过滤：只勾了 anthropic 的规则不得对 OpenAI 账号生效。
func TestOpenAIErrorHandlingRule_AnthropicOnlyRuleDoesNotApply(t *testing.T) {
	svc := newOpenAIRuleService(t, nil, ErrorHandlingRule{
		ID: "anthropic-only", StatusCodes: []int{502}, Keywords: []string{"connection lost"},
		Action: ErrorHandlingActionPassthrough, Platforms: []string{PlatformAnthropic},
	})
	c, _ := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, openAIRuleAccount(), openAILostPingErr, false)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Empty(t, failoverErr.ErrorRuleID, "规则没勾 openai，不能命中")
	require.True(t, failoverErr.ShouldRetryNextAccount(), "未命中时保持内置行为：换号")
}

// 耗时门限：41s 才失败的那一批，图很可能已生成已计费，不该被重试型规则接住。
func TestOpenAIErrorHandlingRule_UpstreamLatencyCeiling(t *testing.T) {
	rule := ErrorHandlingRule{
		ID: "fast-fail-only", StatusCodes: []int{502}, Keywords: []string{"connection lost"},
		Action: ErrorHandlingActionPassthrough, Platforms: []string{PlatformOpenAI},
		MaxUpstreamLatencyMs: 5_000,
	}

	t.Run("耗时低于阈值时命中", func(t *testing.T) {
		svc := newOpenAIRuleService(t, nil, rule)
		c, _ := newOpenAITransportErrTestContext()
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, 504)

		err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, openAIRuleAccount(), openAILostPingErr, false)

		var failoverErr *UpstreamFailoverError
		require.True(t, errors.As(err, &failoverErr))
		require.Equal(t, "fast-fail-only", failoverErr.ErrorRuleID)
	})

	t.Run("耗时高于阈值时不命中", func(t *testing.T) {
		svc := newOpenAIRuleService(t, nil, rule)
		c, _ := newOpenAITransportErrTestContext()
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, 41_000)

		err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, openAIRuleAccount(), openAILostPingErr, false)

		var failoverErr *UpstreamFailoverError
		require.True(t, errors.As(err, &failoverErr))
		require.Empty(t, failoverErr.ErrorRuleID)
	})
}

// 客户端已断开时 handleOpenAIUpstreamTransportError 本来就不换号也不停号；
// 规则不得把这条路径改成 failover。
func TestOpenAIErrorHandlingRule_ContextCanceledStillSkipsRule(t *testing.T) {
	svc := newOpenAIRuleService(t, nil, ErrorHandlingRule{
		ID: "any-502", StatusCodes: []int{502}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformOpenAI},
	})
	c, _ := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, openAIRuleAccount(), context.Canceled, false)

	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "客户端已断开：既不换号也不套规则")
	require.ErrorIs(t, err, context.Canceled)
}

// ==================== 内置独占：规则抢不走 ====================

func TestOpenAIBuiltinOwnsError(t *testing.T) {
	cyberBody := []byte(`{"error":{"code":"cyber_policy","message":"blocked by cyber policy"}}`)
	contextWindowBody := []byte(`{"error":{"message":"Your input exceeds the context window of this model"}}`)

	tests := []struct {
		name       string
		statusCode int
		msg        string
		body       []byte
		account    *Account
		want       bool
	}{
		{
			// request-scoped：换号/重试都是空耗，还误伤凭据
			name: "cyber policy 归内置", statusCode: http.StatusBadRequest,
			msg:  "blocked by cyber policy",
			body: cyberBody, account: openAIRuleAccount(), want: true,
		},
		{
			// 确定性错误，换任何号都复现
			name: "context window 归内置", statusCode: http.StatusBadRequest,
			msg:  "Your input exceeds the context window of this model",
			body: contextWindowBody, account: openAIRuleAccount(), want: true,
		},
		{
			// 跨 switch 的计数器，规则插进去会算乱
			name: "OAuth 账号的 429 归内置", statusCode: http.StatusTooManyRequests,
			msg: "rate limited", body: []byte(`{"error":{"message":"rate limited"}}`),
			account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, want: true,
		},
		{
			name: "API Key 账号的 429 规则可覆盖", statusCode: http.StatusTooManyRequests,
			msg: "rate limited", body: []byte(`{"error":{"message":"rate limited"}}`),
			account: openAIRuleAccount(), want: false,
		},
		{
			name: "普通 502 规则可覆盖", statusCode: http.StatusBadGateway,
			msg: "bad gateway", body: []byte(`{"error":{"message":"bad gateway"}}`),
			account: openAIRuleAccount(), want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, openAIBuiltinOwnsError(tt.statusCode, tt.msg, tt.body, tt.account))
		})
	}
}

// 规则未命中时，转发路径必须一行行为不变：override 报告 handled=false。
func TestOpenAIErrorHandlingRuleOverride_NoMatchLeavesBuiltinPath(t *testing.T) {
	svc := newOpenAIRuleService(t, nil, ErrorHandlingRule{
		ID: "only-429", StatusCodes: []int{429}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformOpenAI},
	})
	c, _ := newOpenAITransportErrTestContext()

	ruleErr, handled := svc.openAIErrorHandlingRuleOverride(
		context.Background(), c, openAIRuleAccount(),
		http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"bad gateway"}}`), "gpt-image-1")

	require.False(t, handled)
	require.Nil(t, ruleErr)
	require.Empty(t, opsUpstreamErrorEvents(t, c), "未命中不得留下规则事件")
}

func TestOpenAIErrorHandlingRuleOverride_HTTPErrorResponseMatches(t *testing.T) {
	svc := newOpenAIRuleService(t, nil, ErrorHandlingRule{
		ID: "http-429", StatusCodes: []int{429}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformOpenAI},
	})
	c, _ := newOpenAITransportErrTestContext()

	ruleErr, handled := svc.openAIErrorHandlingRuleOverride(
		context.Background(), c, openAIRuleAccount(),
		http.StatusTooManyRequests, http.Header{}, []byte(`{"error":{"message":"rate limited"}}`), "gpt-image-1")

	require.True(t, handled)
	require.NotNil(t, ruleErr)
	require.Equal(t, "http-429", ruleErr.ErrorRuleID)
	require.Equal(t, http.StatusTooManyRequests, ruleErr.StatusCode)
}
