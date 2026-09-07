//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// #228 task-7：错误处理规则接入 Gemini 侧。三条转发链（Forward / ForwardNative /
// ForwardAsChatCompletions）共用同一个 *GeminiMessagesCompatService 接收者，
// 接线只需要一个方法，不需要像 OpenAI/Anthropic 那样为多个 service 类型各写一份。

func newGeminiRuleSettingService(t *testing.T, rules ...ErrorHandlingRule) *SettingService {
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

func newGeminiRuleService(t *testing.T, upstream HTTPUpstream, rules ...ErrorHandlingRule) *GeminiMessagesCompatService {
	t.Helper()
	return &GeminiMessagesCompatService{
		httpUpstream:   upstream,
		settingService: newGeminiRuleSettingService(t, rules...),
		cfg:            &config.Config{},
	}
}

func geminiRuleAccount() *Account {
	return &Account{
		ID: 70, Name: "gemini-rule-account", Platform: PlatformGemini, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
	}
}

func newGeminiRuleTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

// ==================== 三种动作：failover / retry / passthrough ====================

func TestGeminiErrorHandlingRuleOverrideActions(t *testing.T) {
	t.Run("failover", func(t *testing.T) {
		svc := newGeminiRuleService(t, nil, ErrorHandlingRule{
			ID: "gemini-failover", StatusCodes: []int{429}, Action: ErrorHandlingActionFailover,
			Platforms: []string{PlatformGemini},
		})
		c, _ := newGeminiRuleTestContext()

		failoverErr, handled := svc.geminiErrorHandlingRuleOverride(context.Background(), c, geminiErrorHandlingRuleInput{
			Account: geminiRuleAccount(), StatusCode: http.StatusTooManyRequests,
			Body:     []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"rate limited"}}`),
			ReqModel: "gemini-2.5-pro", BuiltinWillFailover: true,
		})

		require.True(t, handled)
		require.NotNil(t, failoverErr)
		require.Equal(t, "gemini-failover", failoverErr.ErrorRuleID)
		require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
		require.True(t, failoverErr.ShouldRetryNextAccount())
		require.NotEmpty(t, failoverErr.SafeErrorType)
		require.NotEmpty(t, failoverErr.SafeErrorMessage)
	})

	t.Run("retry", func(t *testing.T) {
		retry := 3
		svc := newGeminiRuleService(t, nil, ErrorHandlingRule{
			ID: "gemini-retry", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry,
			RetryCount: &retry, Platforms: []string{PlatformGemini},
		})
		c, _ := newGeminiRuleTestContext()

		failoverErr, handled := svc.geminiErrorHandlingRuleOverride(context.Background(), c, geminiErrorHandlingRuleInput{
			Account: geminiRuleAccount(), StatusCode: http.StatusInternalServerError,
			Body:     []byte(`{"error":{"status":"INTERNAL","message":"boom"}}`),
			ReqModel: "gemini-2.5-pro", BuiltinWillFailover: true,
		})

		require.True(t, handled)
		require.NotNil(t, failoverErr)
		require.True(t, failoverErr.RetryableOnSameAccount)
		require.NotNil(t, failoverErr.RuleRetryLimit, "规则驱动的重试预算必须显式带出来")
		require.Equal(t, 3, *failoverErr.RuleRetryLimit)
		require.NotEmpty(t, failoverErr.SafeErrorType)
		require.NotEmpty(t, failoverErr.SafeErrorMessage)
	})

	t.Run("passthrough", func(t *testing.T) {
		svc := newGeminiRuleService(t, nil, ErrorHandlingRule{
			ID: "gemini-passthrough", StatusCodes: []int{503}, Action: ErrorHandlingActionPassthrough,
			Platforms: []string{PlatformGemini},
		})
		c, _ := newGeminiRuleTestContext()

		failoverErr, handled := svc.geminiErrorHandlingRuleOverride(context.Background(), c, geminiErrorHandlingRuleInput{
			Account: geminiRuleAccount(), StatusCode: http.StatusServiceUnavailable,
			Body:     []byte(`{"error":{"status":"UNAVAILABLE","message":"overloaded"}}`),
			ReqModel: "gemini-2.5-pro", BuiltinWillFailover: true,
		})

		require.True(t, handled)
		require.NotNil(t, failoverErr)
		require.False(t, failoverErr.ShouldRetryNextAccount(), "passthrough 必须立刻停止换号")
		require.Equal(t, NextAccountStop, failoverErr.NextAccountAction)
		require.Equal(t, ErrorHandlingExhaustedActionPassthrough, failoverErr.ExhaustedAction)
		require.NotEmpty(t, failoverErr.SafeErrorType)
		require.NotEmpty(t, failoverErr.SafeErrorMessage)

		events := opsUpstreamErrorEvents(t, c)
		require.NotEmpty(t, events)
		require.Equal(t, "error_handling_rule_passthrough", events[len(events)-1].Kind)
	})
}

// ==================== 内置独占：Google 项目配置类 400 规则抢不走 ====================

// Forward 上这条错误在接线点**之前**就已经 return（isGoogleProjectConfigError），
// 走 Forward 端到端路径证明的其实是「规则引擎压根没被问到」——geminiBuiltinOwnsError
// 判不判 true 都不影响这条链的结果，这里断言的是双重保险生效前该分支已经拦下了。
func TestGeminiBuiltinOwnsGoogleProjectConfigError(t *testing.T) {
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"status":"INVALID_ARGUMENT","message":"Invalid project resource name (project 123)"}}`)),
		},
	}
	svc := newGeminiRuleService(t, httpStub, ErrorHandlingRule{
		ID: "broad-400", StatusCodes: []int{400}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformGemini},
	})
	c, _ := newGeminiRuleTestContext()
	account := geminiRuleAccount()
	body := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)

	_, err := svc.Forward(context.Background(), c, account, body)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, failoverErr.ErrorRuleID, "内置项目配置类错误 return 在接线点之前，规则引擎不该被问到")
	require.True(t, failoverErr.RetryableOnSameAccount, "内置这条分支的语义是同号可重试")

	events := opsUpstreamErrorEvents(t, c)
	require.NotEmpty(t, events)
	require.Equal(t, "failover", events[len(events)-1].Kind, "不是 error_handling_rule_* 前缀，说明规则引擎没有接管")
}

// geminiBuiltinOwnsError 本身的判定：只认「400 + Google 项目配置类消息」，其余一律
// 交给规则引擎。这个判定在 Forward/ForwardNative 上是冗余的（那两条链在接线点之前
// 就已经 return，走不到这里），但在 ForwardAsChatCompletions 上是唯一的防线——见下面
// TestGeminiErrorHandlingRule_ChatCompletionsChainRefusesGoogleProjectConfigRule。
func TestGeminiBuiltinOwnsError_GoogleProjectConfig(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		msg        string
		want       bool
	}{
		{
			name: "400 + 项目配置错误 归内置", statusCode: http.StatusBadRequest,
			msg: "invalid project resource name (project 123)", want: true,
		},
		{
			name: "同样的消息但状态码不是 400 时不归内置", statusCode: http.StatusForbidden,
			msg: "invalid project resource name (project 123)", want: false,
		},
		{
			name: "400 但消息不匹配时规则可覆盖", statusCode: http.StatusBadRequest,
			msg: "some other invalid argument", want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, geminiBuiltinOwnsError(tt.statusCode, tt.msg, nil))
		})
	}
}

// ForwardAsChatCompletions 没有 Forward/ForwardNative 那两类「接线点之前物理 return」
// 的早退分支（checkErrorPolicyInLoop 只决定同号重试循环要不要 break，从不短路错误
// 处理路径），所以这条链上 geminiBuiltinOwnsError 是唯一挡住规则引擎的防线。这里
// 直接驱动 ForwardAsChatCompletions（而不是单独调用 geminiErrorHandlingRuleOverride
// 或 geminiBuiltinOwnsError）来证明：即便配了一条广泛匹配 400 的 failover 规则，
// 这条链最终返回的错误也不是规则引擎产出的 *UpstreamFailoverError。
func TestGeminiErrorHandlingRule_ChatCompletionsChainRefusesGoogleProjectConfigRule(t *testing.T) {
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"status":"INVALID_ARGUMENT","message":"Invalid project resource name (project 123)"}}`)),
		},
	}
	svc := newGeminiRuleService(t, httpStub, ErrorHandlingRule{
		ID: "broad-400", StatusCodes: []int{400}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformGemini},
	})
	c, rec := newGeminiRuleTestContext()
	account := geminiRuleAccount()
	body := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}],"stream":false}`)

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr),
		"规则引擎产出的错误是 *UpstreamFailoverError；这里必须不是，否则说明规则抢走了"+
			"确定性的 Google 项目配置错误，会让整个账号池被无谓地打一遍换号")
	require.NotEqual(t, http.StatusOK, rec.Code, "内置兜底路径必须已经把错误响应写给客户端")

	events := opsUpstreamErrorEvents(t, c)
	for _, ev := range events {
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind,
			"不该出现规则接管的事件——这条确定性配置错误必须由内置兜底处理")
	}
}

// ==================== 错误透传规则优先 ====================

func newGeminiMatchAllErrorPassthroughService(t *testing.T) *ErrorPassthroughService {
	t.Helper()
	respCode := http.StatusTeapot
	customMessage := "上游请求失败"
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{{
		ID: 1, Name: "gemini-400", Enabled: true, Priority: 1,
		ErrorCodes:   []int{http.StatusBadRequest},
		MatchMode:    model.MatchModeAll,
		Platforms:    []string{PlatformGemini},
		ResponseCode: &respCode, CustomMessage: &customMessage,
		SkipMonitoring: true,
	}})
	return svc
}

func TestGeminiErrorHandlingRuleYieldsToPassthroughRule(t *testing.T) {
	svc := newGeminiRuleService(t, nil, ErrorHandlingRule{
		ID: "broad-400", StatusCodes: []int{400}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformGemini},
	})
	account := geminiRuleAccount()
	body := []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"invalid request"}}`)

	c, rec := newGeminiRuleTestContext()
	BindErrorPassthroughService(c, newGeminiMatchAllErrorPassthroughService(t))
	_, handled := svc.geminiErrorHandlingRuleOverride(context.Background(), c, geminiErrorHandlingRuleInput{
		Account: account, StatusCode: http.StatusBadRequest, Body: body, ReqModel: "gemini-2.5-pro",
		BuiltinWillFailover: false,
	})
	require.False(t, handled, "内置不换号 + 透传规则命中 ⇒ 错误处理规则让路")

	// 让路之后，调用方会继续走 writeGeminiMappedError，那里才真正应用透传规则、
	// 写出最终响应。这里直接调用它来验证最终 HTTP 状态、消息体、OpsSkipPassthroughKey
	// 三者，而不只是 handled=false。
	_ = svc.writeGeminiMappedError(c, account, http.StatusBadRequest, "req-1", body)

	require.Equal(t, http.StatusTeapot, rec.Code)
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "上游请求失败", payload.Error.Message)

	skip, ok := c.Get(OpsSkipPassthroughKey)
	require.True(t, ok, "OpsSkipPassthroughKey 必须被置位，避免下游重复应用透传规则")
	require.Equal(t, true, skip)

	// 内置要换号的分支不受影响：那条分支上本来就问不到透传规则。
	c2, _ := newGeminiRuleTestContext()
	BindErrorPassthroughService(c2, newGeminiMatchAllErrorPassthroughService(t))
	_, handled2 := svc.geminiErrorHandlingRuleOverride(context.Background(), c2, geminiErrorHandlingRuleInput{
		Account: account, StatusCode: http.StatusBadRequest, Body: body, ReqModel: "gemini-2.5-pro",
		BuiltinWillFailover: true,
	})
	require.True(t, handled2)
}

// ==================== 平台过滤 ====================

// 只勾了 anthropic 的规则不得对 Gemini 账号生效。
func TestGeminiErrorHandlingRule_OtherPlatformRuleDoesNotApply(t *testing.T) {
	svc := newGeminiRuleService(t, nil, ErrorHandlingRule{
		ID: "anthropic-only", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformAnthropic},
	})
	c, _ := newGeminiRuleTestContext()

	failoverErr, handled := svc.geminiErrorHandlingRuleOverride(context.Background(), c, geminiErrorHandlingRuleInput{
		Account: geminiRuleAccount(), StatusCode: http.StatusInternalServerError,
		Body:     []byte(`{"error":{"status":"INTERNAL","message":"boom"}}`),
		ReqModel: "gemini-2.5-pro", BuiltinWillFailover: true,
	})

	require.False(t, handled, "规则没勾 gemini，不能命中")
	require.Nil(t, failoverErr)
	require.Empty(t, opsUpstreamErrorEvents(t, c), "未命中不得留下规则事件")
}
