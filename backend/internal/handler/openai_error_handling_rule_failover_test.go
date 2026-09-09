package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// #189：错误处理规则在 OpenAI 侧的两个 handler 落点。
//
// 1. ExhaustedAction=passthrough 在 OpenAI 侧原先完全没人看：全仓只有 GatewayHandler
//    的两处消费它，OpenAIGatewayHandler.handleFailoverExhausted 从头到尾没有这个分支。
// 2. 规则的 retry 预算要能覆盖账号的 pool-mode 预算，否则会被账号基数顶掉——
//    非 pool-mode 或未显式配置时基数是默认值 3，规则配的次数比它大就只会重试
//    3 次；管理员把 pool_mode_retry_count 显式设成 0 时，界面给了「原地重试
//    N 次」而行为会直接换号。

func TestOpenAIFailoverExhausted_RulePassthroughReturnsUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		ResponseBody:      []byte(`{"error":{"type":"rate_limit_error","message":"upstream is rate limited"}}`),
		ErrorRuleID:       "rule-1",
		ExhaustedAction:   service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:     "rate_limit_error",
		SafeErrorMessage:  "upstream is rate limited",
		NextAccountAction: service.NextAccountStop,
	}, false)

	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"passthrough 要把上游状态码原样交给客户端，而不是走 mapUpstreamError")
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	errBody, ok := envelope["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "rate_limit_error", errBody["type"])
	require.Equal(t, "upstream is rate limited", errBody["message"])
}

// 安全阀：SafeErrorType/Message 缺一不可，否则退回内置映射，绝不把裸上游体吐出去。
func TestOpenAIFailoverExhausted_RulePassthroughWithoutSafeErrorFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseBody:    []byte(`{"error":{"message":"secret=must-not-leak"}}`),
		ExhaustedAction: service.ErrorHandlingExhaustedActionPassthrough,
	}, false)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotContains(t, rec.Body.String(), "must-not-leak")
}

// FIX 3（#228 最终评审）：耗尽后走 passthrough 时，如果 StatusCode 是传输层失败
// 合成的虚拟 502（SyntheticStatus=true），不能把它写进 ops_error_logs 顶层的
// upstream_status_code——那一列为 NULL 正是"这是传输层失败、根本没有 HTTP 响应"
// 的判定依据。客户端仍然要拿到这个合成状态码（这是既有行为，FIX 3 不改）。
func TestOpenAIFailoverExhausted_SyntheticStatusNotRecordedAsUpstreamStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:       http.StatusBadGateway,
		ResponseBody:     []byte(`{"error":{"type":"upstream_error","message":"upstream request failed: connection reset"}}`),
		ExhaustedAction:  service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:    "upstream_error",
		SafeErrorMessage: "upstream request failed: connection reset",
		SyntheticStatus:  true,
	}, false)

	// 客户端响应状态码不受影响：仍是合成的 502。
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "connection reset")

	_, ok := c.Get(service.OpsUpstreamStatusCodeKey)
	require.False(t, ok, "合成状态码只能用于客户端响应，不得落进 ops_error_logs 顶层列")
}

func TestEffectiveSameAccountRetryLimit_RuleBudgetOverridesPoolMode(t *testing.T) {
	account := &service.Account{ID: 60, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	// 没有规则预算时沿用账号侧的 pool-mode 预算（非 pool-mode 账号取默认值）。
	poolDefault := effectiveSameAccountRetryLimit(&service.UpstreamFailoverError{
		RetryableOnSameAccount: true,
	}, account)

	limit := poolDefault + 2
	require.Equal(t, limit, effectiveSameAccountRetryLimit(&service.UpstreamFailoverError{
		RetryableOnSameAccount: true, RuleRetryLimit: &limit, ErrorRuleID: "rule-1",
	}, account), "规则预算是管理员显式配的，必须能覆盖账号侧预算（含往上放宽）")

	zero := 0
	require.Equal(t, 0, effectiveSameAccountRetryLimit(&service.UpstreamFailoverError{
		RetryableOnSameAccount: true, RuleRetryLimit: &zero, ErrorRuleID: "rule-1",
	}, account), "规则配 0 次重试 = 命中即换号")
}

func TestSameAccountRetryAllowed_RuleBudget(t *testing.T) {
	limit := 2
	failoverErr := &service.UpstreamFailoverError{
		RetryableOnSameAccount: true, RuleRetryLimit: &limit, ErrorRuleID: "rule-1",
	}
	require.True(t, sameAccountRetryAllowed(failoverErr, 0, limit))
	require.True(t, sameAccountRetryAllowed(failoverErr, 1, limit))
	require.False(t, sameAccountRetryAllowed(failoverErr, 2, limit), "预算耗尽后必须换号")
}

func TestOpenAIWSSameAccountRetryAllowed_RuleBudget(t *testing.T) {
	account := &service.Account{ID: 61, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	limit := 2
	failoverErr := &service.UpstreamFailoverError{
		StatusCode: http.StatusServiceUnavailable, RetryableOnSameAccount: true,
		RuleRetryLimit: &limit, ErrorRuleID: "rule-ws",
	}

	require.True(t, openAIWSSameAccountRetryAllowed(account, failoverErr, 0))
	require.True(t, openAIWSSameAccountRetryAllowed(account, failoverErr, 1))
	require.False(t, openAIWSSameAccountRetryAllowed(account, failoverErr, 2))
	require.False(t, openAIWSSameAccountRetryAllowed(account, &service.UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable}, 0))
}

func TestOpenAIWSFailoverExhausted_RulePassthroughPreservesSyntheticStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	closeOpenAIWSFailoverExhausted(c, nil, &service.UpstreamFailoverError{
		StatusCode:       http.StatusBadGateway,
		ExhaustedAction:  service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:    "upstream_error",
		SafeErrorMessage: "upstream stream interrupted",
		SyntheticStatus:  true,
	})

	streamErr, ok := service.GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, "upstream_error", streamErr.ErrType)
	require.Equal(t, "upstream stream interrupted", streamErr.Message)
	require.Zero(t, streamErr.IntendedStatus, "合成 502 不得伪装成真实上游状态")
	require.True(t, streamErr.CountTowardsSLA)
}

// ==================== PR #194 评审后的补丁覆盖 ====================

// failoverOpenAIUpstreamHTTPError 是三条 *_anthropic_native 路径的汇聚点，而
// /v1/messages 的耗尽走 handleAnthropicFailoverExhausted。少了 ExhaustedAction 分支，
// 规则配的 passthrough 只执行了「不再换号」的一半，客户端仍拿到 mapUpstreamError。
func TestAnthropicFailoverExhausted_RulePassthroughReturnsUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		ResponseBody:      []byte(`{"error":{"type":"rate_limit_error","message":"upstream is rate limited"}}`),
		ErrorRuleID:       "rule-1",
		ExhaustedAction:   service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:     "rate_limit_error",
		SafeErrorMessage:  "upstream is rate limited",
		NextAccountAction: service.NextAccountStop,
	}, false)

	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"passthrough 要把上游状态码原样交给客户端，而不是走 mapUpstreamError")
	require.Contains(t, rec.Body.String(), "upstream is rate limited")
}

// 同一条安全阀：SafeErrorType/Message 缺一不可。
func TestAnthropicFailoverExhausted_RulePassthroughWithoutSafeErrorFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseBody:    []byte(`{"error":{"message":"secret=must-not-leak"}}`),
		ExhaustedAction: service.ErrorHandlingExhaustedActionPassthrough,
	}, false)

	require.NotContains(t, rec.Body.String(), "must-not-leak")
}

// FIX 3（#228 最终评审）：handleAnthropicFailoverExhausted 与 handleFailoverExhausted
// 是同一份 SyntheticStatus 处理逻辑各自的一份拷贝，必须分别钉住。
func TestAnthropicFailoverExhausted_SyntheticStatusNotRecordedAsUpstreamStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:       http.StatusBadGateway,
		ResponseBody:     []byte(`{"error":{"type":"upstream_error","message":"upstream request failed: connection reset"}}`),
		ExhaustedAction:  service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:    "upstream_error",
		SafeErrorMessage: "upstream request failed: connection reset",
		SyntheticStatus:  true,
	}, false)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "connection reset")

	_, ok := c.Get(service.OpsUpstreamStatusCodeKey)
	require.False(t, ok, "合成状态码只能用于客户端响应，不得落进 ops_error_logs 顶层列")
}
