//go:build unit

package service

import (
	"bytes"
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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// #228 task-8：错误处理规则接入 Antigravity 侧。三条"标准"转发链（Forward /
// ForwardGemini / forwardAntigravityCompat）共用同一个 *AntigravityGatewayService
// 接收者，接线只需要一个方法；此外 antigravityRetryLoop 内部还有一个更早的接线点，
// 让规则在首次失败、内置 3 次通用重试耗尽之前就被问到（见 antigravity_error_handling_rule.go
// 顶部注释与 #228 评审修订 §三）。

func newAntigravityRuleSettingService(t *testing.T, rules ...ErrorHandlingRule) *SettingService {
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

func antigravityRuleAccount() *Account {
	return &Account{
		ID: 80, Name: "antigravity-rule-account", Platform: PlatformAntigravity, Type: AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "token",
			"project_id":   "proj",
		},
	}
}

func newAntigravityRuleTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

// ==================== 三种动作：failover / retry / passthrough ====================

func TestAntigravityErrorHandlingRuleOverrideActions(t *testing.T) {
	t.Run("failover", func(t *testing.T) {
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
			ID: "antigravity-failover", StatusCodes: []int{429}, Action: ErrorHandlingActionFailover,
			Platforms: []string{PlatformAntigravity},
		})}
		c, _ := newAntigravityRuleTestContext()

		failoverErr, handled := svc.antigravityErrorHandlingRuleOverride(context.Background(), c, antigravityErrorHandlingRuleInput{
			Account: antigravityRuleAccount(), StatusCode: http.StatusTooManyRequests,
			Body:     []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"rate limited"}}`),
			ReqModel: "claude-sonnet-4-5", BuiltinWillFailover: true,
		})

		require.True(t, handled)
		require.NotNil(t, failoverErr)
		require.Equal(t, "antigravity-failover", failoverErr.ErrorRuleID)
		require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
		require.True(t, failoverErr.ShouldRetryNextAccount())
		require.NotEmpty(t, failoverErr.SafeErrorType)
		require.NotEmpty(t, failoverErr.SafeErrorMessage)
	})

	t.Run("retry", func(t *testing.T) {
		retry := 3
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
			ID: "antigravity-retry", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry,
			RetryCount: &retry, Platforms: []string{PlatformAntigravity},
		})}
		c, _ := newAntigravityRuleTestContext()

		failoverErr, handled := svc.antigravityErrorHandlingRuleOverride(context.Background(), c, antigravityErrorHandlingRuleInput{
			Account: antigravityRuleAccount(), StatusCode: http.StatusInternalServerError,
			Body:     []byte(`{"error":{"status":"INTERNAL","message":"boom"}}`),
			ReqModel: "claude-sonnet-4-5", BuiltinWillFailover: true,
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
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
			ID: "antigravity-passthrough", StatusCodes: []int{503}, Action: ErrorHandlingActionPassthrough,
			Platforms: []string{PlatformAntigravity},
		})}
		c, _ := newAntigravityRuleTestContext()

		failoverErr, handled := svc.antigravityErrorHandlingRuleOverride(context.Background(), c, antigravityErrorHandlingRuleInput{
			Account: antigravityRuleAccount(), StatusCode: http.StatusServiceUnavailable,
			Body:     []byte(`{"error":{"status":"UNAVAILABLE","message":"overloaded"}}`),
			ReqModel: "claude-sonnet-4-5", BuiltinWillFailover: true,
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

// ==================== 内置独占：Google 项目配置类 400（Forward，双重保险） ====================

// Forward 上这条错误在接线点**之前**就已经 return（isGoogleProjectConfigError），
// 走 Forward 端到端路径证明的其实是「规则引擎压根没被问到」——antigravityBuiltinOwnsError
// 判不判 true 都不影响这条链的结果，这里断言的是双重保险生效前该分支已经拦下了。
func TestAntigravityBuiltinOwnsGoogleProjectConfigError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	respBody := []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"Invalid project resource name (project 123)"}}`)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"X-Request-Id": []string{"req-1"}},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	svc := &AntigravityGatewayService{
		settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
			ID: "broad-400", StatusCodes: []int{400}, Action: ErrorHandlingActionPassthrough,
			Platforms: []string{PlatformAntigravity},
		}),
		tokenProvider: &AntigravityTokenProvider{},
		httpUpstream:  &httpUpstreamStub{resp: resp},
	}

	account := antigravityRuleAccount()
	body, err := json.Marshal(map[string]any{
		"model":      "claude-opus-4-6",
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		"max_tokens": 64,
		"stream":     false,
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	_, forwardErr := svc.Forward(context.Background(), c, account, body, false)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, forwardErr, &failoverErr)
	require.Empty(t, failoverErr.ErrorRuleID, "内置项目配置类错误 return 在接线点之前，规则引擎不该被问到")
	require.True(t, failoverErr.RetryableOnSameAccount, "内置这条分支的语义是同号可重试")

	events := opsUpstreamErrorEvents(t, c)
	require.NotEmpty(t, events)
	require.Equal(t, "failover", events[len(events)-1].Kind, "不是 error_handling_rule_* 前缀，说明规则引擎没有接管")
}

// antigravityBuiltinOwnsError 本身的判定：只认「400 + Google 项目配置类消息」，其余一律
// 交给规则引擎。这个判定在 Forward/ForwardGemini 上是冗余的（那两条链在接线点之前就已经
// return，走不到这里），但在 forwardAntigravityCompat 上是唯一的防线——见下面
// TestAntigravityErrorHandlingRule_CompatChainRefusesGoogleProjectConfigRule。
func TestAntigravityBuiltinOwnsError_GoogleProjectConfig(t *testing.T) {
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
		{
			// fix round 1（#228 评审）：401 凭据被拒无条件归内置独占，不看消息内容——
			// 与 antigravityCredentialRejectedError 自己的分支条件（只看状态码）逐字一致。
			name: "401 无条件归内置，不看消息内容", statusCode: http.StatusUnauthorized,
			msg: "", want: true,
		},
		{
			name:       "401 即便消息形如项目配置错误也仍归内置（走的是 401 分支，不是 400 分支）",
			statusCode: http.StatusUnauthorized,
			msg:        "invalid project resource name (project 123)", want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, antigravityBuiltinOwnsError(tt.statusCode, tt.msg, nil))
		})
	}
}

// ==================== 内置独占：Google 项目配置类 400（compat 链，承重） ====================

// forwardAntigravityCompat（ForwardAsChatCompletions / ForwardAsResponses 共用
// handleAntigravityCompatHTTPError）没有 Forward/ForwardGemini 那两类「接线点之前物理
// return」的早退分支，所以这条链上 antigravityBuiltinOwnsError 是唯一挡住规则引擎的
// 防线。这里直接驱动 ForwardAsChatCompletions（而不是单独调用
// antigravityErrorHandlingRuleOverride 或 antigravityBuiltinOwnsError）来证明：即便配了
// 一条广泛匹配 400 的 failover 规则，这条链最终返回的错误也不是规则引擎产出的
// *UpstreamFailoverError。
func TestAntigravityErrorHandlingRule_CompatChainRefusesGoogleProjectConfigRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	respBody := []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"Invalid project resource name (project 123)"}}`)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"X-Request-Id": []string{"req-2"}},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	svc := newAntigravityCompatService(config.GatewayConfig{}, &httpUpstreamStub{resp: resp})
	svc.settingService = newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "broad-400", StatusCodes: []int{400}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformAntigravity},
	})

	account := newAntigravityCompatAccount(AccountTypeOAuth)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	c, rec := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)

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

// ==================== 内置独占：401 凭据被拒（compat 链，承重，fix round 1） ====================

// #228 评审 fix round 1：handleAntigravityCompatHTTPError 的 401 分支
// （antigravityCredentialRejectedError）产出的是带 Stage/Scope/Reason/ClientMessage
// 等类型字段的 UpstreamFailoverError，这些字段是下游归因（ops_service.go 的
// Stage==AccountAuth 判断）与账号池耗尽时客户端文案（credentialFailoverClientResponse
// 按 Reason 查表，查不到会落到另一个平台的 GrokCredentialUnavailableClientMessage）的
// 唯一来源。规则版错误带不出这些字段——如果一条形如「401 → failover」的管理台规则能
// 抢先接管，这些字段会被清零。这里驱动 ForwardAsChatCompletions 端到端证明：即便配了
// 这样一条规则，返回的错误仍然是 antigravityCredentialRejectedError 产出的那个，类型
// 字段原样保留，而不是规则引擎产出的裸 UpstreamFailoverError。
func TestAntigravityErrorHandlingRule_CompatChainRefusesCredentialRejectionRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	respBody := []byte(`{"error":{"status":"UNAUTHENTICATED","message":"Request had invalid authentication credentials"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"X-Request-Id": []string{"req-3"}},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	svc := newAntigravityCompatService(config.GatewayConfig{}, &httpUpstreamStub{resp: resp})
	svc.settingService = newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "broad-401", StatusCodes: []int{401}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformAntigravity},
	})

	account := newAntigravityCompatAccount(AccountTypeOAuth)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "凭据被拒必须仍然产出带类型字段的 UpstreamFailoverError")
	require.Empty(t, failoverErr.ErrorRuleID,
		"ErrorRuleID 非空说明规则引擎抢走了这个错误——凭据被拒的类型字段会因此被清零")
	require.Equal(t, GatewayFailureStageAccountAuth, failoverErr.Stage)
	require.Equal(t, GatewayFailureScopeAccount, failoverErr.Scope)
	require.Equal(t, AntigravityCredentialRejectedReason, failoverErr.Reason)
	require.Equal(t, AntigravityCredentialRejectedClientMessage, failoverErr.ClientMessage)
	require.Equal(t, http.StatusBadGateway, failoverErr.ClientStatusCode)

	events := opsUpstreamErrorEvents(t, c)
	for _, ev := range events {
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind,
			"不该出现规则接管的事件——401 凭据被拒必须由内置的 antigravityCredentialRejectedError 处理")
	}
}

// ==================== 错误透传规则优先 ====================

func newAntigravityMatchAllErrorPassthroughService(t *testing.T) *ErrorPassthroughService {
	t.Helper()
	respCode := http.StatusTeapot
	customMessage := "上游请求失败"
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{{
		ID: 1, Name: "antigravity-400", Enabled: true, Priority: 1,
		ErrorCodes:   []int{http.StatusBadRequest},
		MatchMode:    model.MatchModeAll,
		Platforms:    []string{PlatformAntigravity},
		ResponseCode: &respCode, CustomMessage: &customMessage,
		SkipMonitoring: true,
	}})
	return svc
}

// TestAntigravityErrorHandlingRuleWinsOverPassthroughRule 验证 2026-09-08 项目
// 所有者的反转决定：两个机制同时命中同一个错误时，错误处理规则引擎胜出，不再像
// 旧语义那样让路给错误透传规则（旧名 TestAntigravityErrorHandlingRuleYieldsToPassthroughRule）。
func TestAntigravityErrorHandlingRuleWinsOverPassthroughRule(t *testing.T) {
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "broad-400", StatusCodes: []int{400}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformAntigravity},
	})}
	account := antigravityRuleAccount()
	body := []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"invalid request"}}`)

	c, _ := newAntigravityRuleTestContext()
	// 错误透传规则也命中同一个 400——证明两者同时命中时规则引擎胜出，而不是像
	// 旧语义那样在这个分支上让路。
	BindErrorPassthroughService(c, newAntigravityMatchAllErrorPassthroughService(t))
	failoverErr, handled := svc.antigravityErrorHandlingRuleOverride(context.Background(), c, antigravityErrorHandlingRuleInput{
		Account: account, StatusCode: http.StatusBadRequest, Body: body, ReqModel: "claude-sonnet-4-5",
		BuiltinWillFailover: false,
	})
	require.True(t, handled, "两个机制同时命中时错误处理规则引擎胜出（2026-09-08 反转 #228 非目标）")
	require.NotNil(t, failoverErr)
	require.Equal(t, "broad-400", failoverErr.ErrorRuleID)
	require.Equal(t, ErrorHandlingExhaustedActionPassthrough, failoverErr.ExhaustedAction)
	require.Equal(t, NextAccountStop, failoverErr.NextAccountAction)
	// SafeErrorType/Message 来自规则引擎自己的 safeAntigravityError（从原始上游
	// body 里取），不是透传规则改写后的 CustomMessage："上游请求失败"——用来证明
	// 命中的确实是规则引擎的结果，透传规则连改写的机会都没有。
	require.Equal(t, "INVALID_ARGUMENT", failoverErr.SafeErrorType)
	require.Equal(t, "invalid request", failoverErr.SafeErrorMessage)

	// OpsSkipPassthroughKey 只由 applyErrorPassthroughRule 置位；规则引擎胜出这条
	// 路径从未调用它，这里必须是未置位——否则说明透传规则偷偷跑过了。
	_, skipSet := c.Get(OpsSkipPassthroughKey)
	require.False(t, skipSet, "规则引擎胜出时不应该经过 applyErrorPassthroughRule")

	// 内置要换号的分支不受影响：不管透传规则命不命中，规则引擎该赢还是赢。
	c2, _ := newAntigravityRuleTestContext()
	BindErrorPassthroughService(c2, newAntigravityMatchAllErrorPassthroughService(t))
	_, handled2 := svc.antigravityErrorHandlingRuleOverride(context.Background(), c2, antigravityErrorHandlingRuleInput{
		Account: account, StatusCode: http.StatusBadRequest, Body: body, ReqModel: "claude-sonnet-4-5",
		BuiltinWillFailover: true,
	})
	require.True(t, handled2)
}

// TestWriteMappedClaudeError_PassthroughRuleAloneStillApplies 是"透传规则单独命中
// 仍然生效"的守护测试：svc 没有绑定 settingService，错误处理规则引擎结构性不可能
// 命中，只有错误透传规则命中。上面的反转只改变"两者同时命中"这一种碰撞，这里必须
// 保持原样——这条测试原本隐含在 TestAntigravityErrorHandlingRuleYieldsToPassthroughRule
// 的第二段里，反转后单独补一个，避免把"透传规则单独生效"这条覆盖率一并反转丢了。
func TestWriteMappedClaudeError_PassthroughRuleAloneStillApplies(t *testing.T) {
	svc := &AntigravityGatewayService{}
	account := antigravityRuleAccount()
	body := []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"invalid request"}}`)

	c, rec := newAntigravityRuleTestContext()
	BindErrorPassthroughService(c, newAntigravityMatchAllErrorPassthroughService(t))

	_ = svc.writeMappedClaudeError(c, account, http.StatusBadRequest, "req-1", body)

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
}

// ==================== 平台过滤 ====================

// 只勾了 anthropic 的规则不得对 Antigravity 账号生效。
func TestAntigravityErrorHandlingRule_OtherPlatformRuleDoesNotApply(t *testing.T) {
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "anthropic-only", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformAnthropic},
	})}
	c, _ := newAntigravityRuleTestContext()

	failoverErr, handled := svc.antigravityErrorHandlingRuleOverride(context.Background(), c, antigravityErrorHandlingRuleInput{
		Account: antigravityRuleAccount(), StatusCode: http.StatusInternalServerError,
		Body:     []byte(`{"error":{"status":"INTERNAL","message":"boom"}}`),
		ReqModel: "claude-sonnet-4-5", BuiltinWillFailover: true,
	})

	require.False(t, handled, "规则没勾 antigravity，不能命中")
	require.Nil(t, failoverErr)
	require.Empty(t, opsUpstreamErrorEvents(t, c), "未命中不得留下规则事件")
}

// ==================== OAuth 专用纠错特例独占：MODEL_CAPACITY_EXHAUSTED ====================

// MODEL_CAPACITY_EXHAUSTED 走 handleSmartRetry 的固定 60 次 × 1s 重试（模型级共享容量池，
// 换号无意义），是三个"纠错特例"之一，必须对规则引擎保持独占——规则引擎的早接线钩子只在
// smartRetryActionContinue（纠错特例都没命中）之后才会被问到。这里直接跑
// antigravityRetryLoop，证明命中 MODEL_CAPACITY_EXHAUSTED 时 ruleOverride 钩子从未被调用。
func TestAntigravitySmartRetryOwnsCapacityExhausted(t *testing.T) {
	account := antigravityRuleAccount()

	capacityBody := []byte(`{
		"error": {
			"code": 503,
			"status": "UNAVAILABLE",
			"message": "No capacity available",
			"details": [
				{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "metadata": {"model": "claude-sonnet-4-5"}, "reason": "MODEL_CAPACITY_EXHAUSTED"},
				{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.1s"}
			]
		}
	}`)
	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}

	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(capacityBody))},
		successResp,
	}}

	var ruleAsked bool
	svc := &AntigravityGatewayService{}
	hook := func(statusCode int, header http.Header, body []byte) (*UpstreamFailoverError, bool) {
		ruleAsked = true
		return nil, false
	}

	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "streamGenerateContent",
		body:           []byte(`{"input":"test"}`),
		httpUpstream:   upstream,
		requestedModel: "claude-sonnet-4-5",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
		ruleOverride: hook,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.resp)
	require.Equal(t, http.StatusOK, result.resp.StatusCode)
	require.False(t, ruleAsked, "MODEL_CAPACITY_EXHAUSTED 是 OAuth 专用纠错特例，必须由 handleSmartRetry 独占处理，规则引擎不该被问到")
	require.Equal(t, 2, upstream.callCount, "第1次拿到容量不足，第2次(handleSmartRetry内部固定重试)成功，不应经过通用重试逻辑")
}

// ==================== 早接线点：首次失败即问，不等内置 3 次通用重试耗尽 ====================

// 规则必须在 antigravityRetryLoop 主循环对普通 429/5xx 做内置 3 次通用重试**之前**被
// 问到（#228 评审修订 §三）。用计数上游证明：命中规则时全程只发出 1 次上游请求——如果
// 钩子接线错了位置（放在通用重试之后），第 2 次上游调用会触发
// queuedHTTPUpstreamStub 的 "unexpected upstream call" 错误，从而让下面的
// ErrorAs 断言失败，测试会用一个明确的错误而不是错误的调用次数失败。
func TestAntigravityRuleAppliesBeforeGenericRetry(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
	}{
		{
			name:       "429 走智能重试 continue 分支之后的早接线点",
			statusCode: http.StatusTooManyRequests,
			body:       []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Too many requests"}}`),
		},
		{
			name:       "502 走 shouldRetryAntigravityError 分支的早接线点",
			statusCode: http.StatusBadGateway,
			body:       []byte(`{"error":{"code":502,"status":"UNAVAILABLE","message":"Bad gateway"}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := antigravityRuleAccount()
			svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
				ID: "early-failover", StatusCodes: []int{tt.statusCode}, Action: ErrorHandlingActionFailover,
				Platforms: []string{PlatformAntigravity},
			})}
			c, _ := newAntigravityRuleTestContext()

			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				{StatusCode: tt.statusCode, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(tt.body))},
			}}

			result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
				ctx:            context.Background(),
				prefix:         "[test]",
				account:        account,
				accessToken:    "token",
				action:         "streamGenerateContent",
				body:           []byte(`{"input":"test"}`),
				c:              c,
				httpUpstream:   upstream,
				requestedModel: "claude-sonnet-4-5",
				handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
					return nil
				},
				ruleOverride: svc.antigravityEarlyRuleOverrideHook(context.Background(), c, account, "claude-sonnet-4-5"),
			})

			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr, "命中规则应当直接短路成 *UpstreamFailoverError，不应落到内置的重试用尽/请求失败分支")
			require.Equal(t, "early-failover", failoverErr.ErrorRuleID)
			require.Equal(t, tt.statusCode, failoverErr.StatusCode)
			require.Equal(t, 1, upstream.callCount, "规则在首次失败就该接管，不应因为内置的通用重试而产生第 2 次上游请求")
		})
	}
}

// ==================== #228 task-10：传输层错误合成 502 ====================

// alwaysFailAntigravityUpstream 的 Do 每次调用都返回同一个连接失败错误——模拟
// antigravityRetryLoop 里"同账号重试 + URL fallback 全部耗尽，每次都是同一个连接
// 失败"的场景。可选 cancelAfter/cancel：在第 N 次调用返回之前触发 context 取消，
// 用来精确复现"客户端在最后一次上游调用期间断连，但循环顶部的 ctx.Done() 检查在那
// 一次调用开始前还没观察到取消"这个时序缝隙——这正是
// antigravityTransportErrorRuleOverride 调用点前 `p.ctx.Err() == nil` 判断要单独
// 兜底的那条路径。
type alwaysFailAntigravityUpstream struct {
	err         error
	calls       int
	cancel      context.CancelFunc
	cancelAfter int
}

func (u *alwaysFailAntigravityUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	if u.cancel != nil && u.calls == u.cancelAfter {
		u.cancel()
	}
	return nil, u.err
}

func (u *alwaysFailAntigravityUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func antigravityTransportErrTestUpstream() *alwaysFailAntigravityUpstream {
	return &alwaysFailAntigravityUpstream{err: errors.New("dial tcp: connection reset by peer")}
}

func antigravityRetryLoopBaseParams(account *Account, upstream HTTPUpstream, c *gin.Context) antigravityRetryLoopParams {
	return antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "streamGenerateContent",
		body:           []byte(`{"input":"test"}`),
		c:              c,
		httpUpstream:   upstream,
		requestedModel: "claude-sonnet-4-5",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}
}

// 传输层错误必须合成 502 交给规则匹配，但合成状态码绝不能写进
// ops_error_logs.upstream_status_code。直接调用
// antigravityTransportErrorRuleOverride（而不是驱动整条 antigravityRetryLoop）：
// 与 OpenAI/Gemini 侧同一层级，快且不依赖 sleepAntigravityBackoffWithContext 的
// 真实退避耗时。
func TestAntigravityTransportErrorSynthesizes502WithoutPollutingOpsStatus(t *testing.T) {
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "antigravity-lost-ping", Name: "antigravity 连接丢失换号",
		StatusCodes: []int{502}, Action: ErrorHandlingActionFailover, Platforms: []string{PlatformAntigravity},
	})}
	c, _ := newAntigravityRuleTestContext()

	failoverErr := svc.antigravityTransportErrorRuleOverride(context.Background(), c, antigravityRuleAccount(), "connection reset by peer")

	require.NotNil(t, failoverErr, "规则应命中，产出规则版 failover 错误")
	require.Equal(t, "antigravity-lost-ping", failoverErr.ErrorRuleID)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)

	_, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, ok, "合成状态码只用于匹配，不得落进 ops_error_logs 顶层列")

	events := opsUpstreamErrorEvents(t, c)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "error_handling_rule_failover", last.Kind)
	require.Zero(t, last.UpstreamStatusCode)
}

func TestAntigravityTransportErrorRuleOverride_NoMatchReturnsNil(t *testing.T) {
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "only-429", StatusCodes: []int{429}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformAntigravity},
	})}
	c, _ := newAntigravityRuleTestContext()

	failoverErr := svc.antigravityTransportErrorRuleOverride(context.Background(), c, antigravityRuleAccount(), "connection reset by peer")

	require.Nil(t, failoverErr, "规则只勾了 429，合成的 502 不该命中")
	require.Empty(t, opsUpstreamErrorEvents(t, c), "未命中不得留下规则事件")
}

// ==================== #228 task-10：antigravityRetryLoop 的实际接线点 ====================
//
// 唯一调用点在 antigravityRetryLoop 内部——URL fallback 耗尽后、同账号通用退避
// 之前（见 antigravity_gateway_retry.go）。这一处接线同时覆盖 ForwardGemini 与
// forwardAntigravityCompat/handleAntigravityCompatTransportError 两条转发链，
// 它们都已经在这个 err 上先判过 `err.(*UpstreamFailoverError)`，所以直接驱动
// antigravityRetryLoop 就足以证明两条链都会生效，不需要分别驱动 ForwardGemini 和
// forwardAntigravityCompat 各自的账号/凭据/上游全链路。

func TestAntigravityRetryLoop_TransportErrorRuleTakesEffect(t *testing.T) {
	upstream := antigravityTransportErrTestUpstream()
	account := antigravityRuleAccount()
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "retry-loop-lost-ping", StatusCodes: []int{502}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformAntigravity},
	})}
	c, _ := newAntigravityRuleTestContext()

	result, err := svc.antigravityRetryLoop(antigravityRetryLoopBaseParams(account, upstream, c))

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "retry-loop-lost-ping", failoverErr.ErrorRuleID)
	require.Equal(t, 1, upstream.calls, "规则命中后必须在首次失败停止通用退避重试")

	_, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, ok, "合成状态码不得落进 ops_error_logs 顶层列")
}

// 规则未命中时，存量行为必须零变化：antigravityRetryLoop 原样返回
// "upstream request failed after retries: ..." 包裹错误，不是规则版
// *UpstreamFailoverError；ForwardGemini/forwardAntigravityCompat 据此走各自原有
// 的 writeGoogleError/writeAntigravityCompatError 兜底路径。
func TestAntigravityRetryLoop_TransportErrorNoRuleUnchangedOutput(t *testing.T) {
	upstream := antigravityTransportErrTestUpstream()
	account := antigravityRuleAccount()
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "only-429", StatusCodes: []int{429}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformAntigravity},
	})}
	c, _ := newAntigravityRuleTestContext()

	result, err := svc.antigravityRetryLoop(antigravityRetryLoopBaseParams(account, upstream, c))

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "未命中时不得产出规则版错误")
	require.ErrorIs(t, err, upstream.err, "原始连接错误必须原样被 %w 包裹，调用方的 errors.Is/文案不能变")
	require.Contains(t, err.Error(), "upstream request failed after retries")
	require.Equal(t, antigravityMaxRetries, upstream.calls)

	_, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, ok)
}

// ==================== #228 task-10：客户端断连必须排除在规则匹配之外 ====================
//
// antigravityRetryLoop 顶部的 `<-p.ctx.Done()` 检查只覆盖请求发出前的取消；
// 这里让取消发生在首次 Do() 调用返回之后——顶部检查看不到这次取消，只有紧邻
// antigravityTransportErrorRuleOverride 调用点前的 `p.ctx.Err() == nil` 单独判断
// 能兜住它。
func TestAntigravityRetryLoop_TransportError_ClientDisconnectedSkipsRule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	upstream := antigravityTransportErrTestUpstream()
	upstream.cancel = cancel
	upstream.cancelAfter = 1

	account := antigravityRuleAccount()
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "any-502", StatusCodes: []int{502}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformAntigravity},
	})}
	c, _ := newAntigravityRuleTestContext()
	params := antigravityRetryLoopBaseParams(account, upstream, c)
	params.ctx = ctx

	result, err := svc.antigravityRetryLoop(params)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "客户端已断开：不能被规则接管")
	require.Equal(t, 1, upstream.calls)
	for _, ev := range opsUpstreamErrorEvents(t, c) {
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind, "不该出现规则接管的事件")
	}
}

// ==================== #228 task-10：handleAntigravityCompatTransportError 的
// 账号切换（typed-field）分支不受影响 ====================
//
// AntigravityAccountSwitchError 是模型限流驱动的账号切换信号，来自
// handleSmartRetry，与本任务接线的"Do() 失败、重试用尽"路径结构上不相交——
// forwardAntigravityCompat 在拿到 antigravityRetryLoop 的返回错误后，先判
// `err.(*UpstreamFailoverError)`（本任务新增的规则版错误会在这里被直接放行），
// 剩下没有被那一判命中的错误才会落到 handleAntigravityCompatTransportError，而
// AntigravityAccountSwitchError 从不是 *UpstreamFailoverError，所以两条路径永不
// 交叉。这里直接调用 handleAntigravityCompatTransportError 证明：即便配了一条
// 宽泛能匹配 502/503 的 failover 规则，账号切换信号的 typed 字段
// （ForceCacheBilling 来自 IsStickySession）依然原样带出，规则引擎从未被问到。
func TestAntigravityCompatTransportError_AccountSwitchRetainsTypedFields(t *testing.T) {
	svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, ErrorHandlingRule{
		ID: "broad-failover", StatusCodes: []int{502, 503}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformAntigravity},
	})}
	c, _ := newAntigravityRuleTestContext()
	switchErr := &AntigravityAccountSwitchError{
		OriginalAccountID: 80, RateLimitedModel: "claude-sonnet-4-5", IsStickySession: true,
	}

	err := svc.handleAntigravityCompatTransportError(c, switchErr)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
	require.True(t, failoverErr.ForceCacheBilling, "IsStickySession=true 必须原样带出到 ForceCacheBilling")
	require.Empty(t, failoverErr.ErrorRuleID, "账号切换信号是独立类型，规则引擎从未被问到，不能被规则版错误顶替")
	require.Empty(t, opsUpstreamErrorEvents(t, c), "规则引擎没有被问到，不该留下任何规则事件")
}
