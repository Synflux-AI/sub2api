//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
	"github.com/stretchr/testify/require"
)

// traceRetryAccount 构造一个开启（或关闭）trace_id_passthrough 的 Antigravity OAuth 账号。
func traceRetryAccount(enabled bool) *Account {
	acc := &Account{
		ID:          77,
		Name:        "acc-trace-retry",
		Type:        AccountTypeOAuth,
		Platform:    PlatformAntigravity,
		Concurrency: 1,
	}
	if enabled {
		acc.Extra = map[string]any{"trace_id_passthrough": true}
	}
	return acc
}

// modelCapacity503Body 是触发单账号原地重试（503 + retryDelay >= 阈值）的上游响应体。
func modelCapacity503Body() []byte {
	return []byte(`{
		"error": {
			"code": 503,
			"status": "UNAVAILABLE",
			"details": [
				{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "metadata": {"model": "gemini-3-pro-high"}, "reason": "MODEL_CAPACITY_EXHAUSTED"},
				{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "39s"}
			],
			"message": "No capacity available"
		}
	}`)
}

// TestAntigravitySingleAccountRetry_InjectsTraceHeaderOnRetryAttempt 是本次修复的核心断言：
// 首次请求由 ForwardUpstream 注入，而重试路径会重建 req——修复前重试请求不带 X-Trace-Id，
// 恰好在排查失败请求时丢掉链路。这里断言重试那一次出站请求上确实带头。
//
// 覆盖的是**智能重试**落点（antigravity_gateway_retry.go:217）：MODEL_CAPACITY_EXHAUSTED
// 让 shouldTriggerAntigravitySmartRetry 返回 shouldRateLimitModel=false，因此走智能重试
// 分支而非单账号原地重试。原地重试落点由
// TestAntigravitySingleAccountRetryInPlace_InjectsTraceHeader 直接覆盖。
func TestAntigravitySingleAccountRetry_InjectsTraceHeaderOnRetryAttempt(t *testing.T) {
	const traceValue = "trace-retry-attempt-1"

	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"result":"ok"}`)),
	}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{successResp},
		errors:    []error{nil},
	}

	respBody := modelCapacity503Body()
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	ctx := context.WithValue(ctxWithSingleAccountRetry(), ctxkey.TraceID, traceValue)
	params := antigravityRetryLoopParams{
		ctx:          ctx,
		prefix:       "[test]",
		account:      traceRetryAccount(true),
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  &stubAntigravityAccountRepo{},
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}

	svc := &AntigravityGatewayService{}
	result := svc.handleSmartRetry(params, resp, respBody, "https://ag-1.test", 0, []string{"https://ag-1.test"})

	require.NotNil(t, result)
	require.NotNil(t, result.resp, "原地重试应返回成功响应")
	require.GreaterOrEqual(t, len(upstream.requestHeaders), 1, "应发生至少一次重试请求")

	for i, h := range upstream.requestHeaders {
		require.Equal(t, traceValue, h.Get(traceid.Header),
			"第 %d 次重试请求应带 X-Trace-Id", i+1)
		require.Len(t, h.Values(traceid.Header), 1, "X-Trace-Id 应恰好一份")
	}
}

// TestAntigravitySingleAccountRetryInPlace_InjectsTraceHeader 直接调用
// handleSingleAccountRetryInPlace，覆盖 antigravity_gateway_retry.go 单账号 503 原地重试
// 落点（:395）。必须直接调用：走 handleSmartRetry 时 MODEL_CAPACITY_EXHAUSTED 会在
// shouldTriggerAntigravitySmartRetry 里让 shouldRateLimitModel=false，从而短路到智能重试
// 分支（:217），永远到不了这里。
func TestAntigravitySingleAccountRetryInPlace_InjectsTraceHeader(t *testing.T) {
	const traceValue = "trace-in-place-1"

	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"result":"ok"}`)),
	}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{successResp},
		errors:    []error{nil},
	}

	respBody := modelCapacity503Body()
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	params := antigravityRetryLoopParams{
		ctx:          context.WithValue(ctxWithSingleAccountRetry(), ctxkey.TraceID, traceValue),
		prefix:       "[test]",
		account:      traceRetryAccount(true),
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  &stubAntigravityAccountRepo{},
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}

	svc := &AntigravityGatewayService{}
	result := svc.handleSingleAccountRetryInPlace(params, resp, respBody, "https://ag-1.test", 0, "gemini-3-pro-high")

	require.NotNil(t, result)
	require.NotNil(t, result.resp, "原地重试应返回成功响应")
	require.Len(t, upstream.requestHeaders, 1, "应恰好发生一次原地重试请求")
	require.Equal(t, traceValue, upstream.requestHeaders[0].Get(traceid.Header))
	require.Len(t, upstream.requestHeaders[0].Values(traceid.Header), 1, "X-Trace-Id 应恰好一份")
}

// TestAntigravitySingleAccountRetryInPlace_NoTraceHeaderWhenToggleOff 对照组。
func TestAntigravitySingleAccountRetryInPlace_NoTraceHeaderWhenToggleOff(t *testing.T) {
	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"result":"ok"}`)),
	}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{successResp},
		errors:    []error{nil},
	}

	respBody := modelCapacity503Body()
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	params := antigravityRetryLoopParams{
		ctx:          context.WithValue(ctxWithSingleAccountRetry(), ctxkey.TraceID, "trace-should-not-leak"),
		prefix:       "[test]",
		account:      traceRetryAccount(false),
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  &stubAntigravityAccountRepo{},
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}

	svc := &AntigravityGatewayService{}
	svc.handleSingleAccountRetryInPlace(params, resp, respBody, "https://ag-1.test", 0, "gemini-3-pro-high")

	require.Len(t, upstream.requestHeaders, 1)
	require.Empty(t, upstream.requestHeaders[0].Get(traceid.Header))
}

// TestAntigravityCreditsOveragesRetry_InjectsTraceHeader 覆盖积分超量重试落点：
// 该路径也是独立重建 req（注入 enabledCreditTypes 后重发），修复前同样丢头。
func TestAntigravityCreditsOveragesRetry_InjectsTraceHeader(t *testing.T) {
	const traceValue = "trace-credits-1"

	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{successResp},
		errors:    []error{nil},
	}

	account := traceRetryAccount(true)
	account.Extra["allow_overages"] = true

	respBody := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	params := antigravityRetryLoopParams{
		ctx:            context.WithValue(context.Background(), ctxkey.TraceID, traceValue),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "generateContent",
		body:           []byte(`{"model":"claude-opus-4-6","request":{}}`),
		httpUpstream:   upstream,
		accountRepo:    &stubAntigravityAccountRepo{},
		requestedModel: "claude-opus-4-6",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}

	svc := &AntigravityGatewayService{}
	result := svc.handleSmartRetry(params, resp, respBody, "https://ag-1.test", 0, []string{"https://ag-1.test"})

	require.NotNil(t, result)
	require.NotNil(t, result.resp)
	require.Len(t, upstream.requestHeaders, 1, "应恰好发生一次积分重试请求")
	require.Contains(t, string(upstream.requestBodies[0]), "enabledCreditTypes")
	require.Equal(t, traceValue, upstream.requestHeaders[0].Get(traceid.Header))
}

// TestAntigravitySingleAccountRetry_NoTraceHeaderWhenToggleOff 对照组：
// 开关关闭时重试请求上不得出现该头（CLI 指纹字节级一致性）。
func TestAntigravitySingleAccountRetry_NoTraceHeaderWhenToggleOff(t *testing.T) {
	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"result":"ok"}`)),
	}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{successResp},
		errors:    []error{nil},
	}

	respBody := modelCapacity503Body()
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}

	ctx := context.WithValue(ctxWithSingleAccountRetry(), ctxkey.TraceID, "trace-should-not-leak")
	params := antigravityRetryLoopParams{
		ctx:          ctx,
		prefix:       "[test]",
		account:      traceRetryAccount(false),
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  &stubAntigravityAccountRepo{},
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}

	svc := &AntigravityGatewayService{}
	svc.handleSmartRetry(params, resp, respBody, "https://ag-1.test", 0, []string{"https://ag-1.test"})

	require.GreaterOrEqual(t, len(upstream.requestHeaders), 1, "应发生至少一次重试请求")
	for i, h := range upstream.requestHeaders {
		require.Empty(t, h.Get(traceid.Header), "开关关闭时第 %d 次重试不应带 X-Trace-Id", i+1)
	}
}
