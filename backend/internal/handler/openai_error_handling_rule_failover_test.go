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
// 2. 规则的 retry 预算要能覆盖账号的 pool-mode 预算，否则非 pool-mode 账号上
//    界面给了「原地重试 N 次」而行为是直接换号。

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
