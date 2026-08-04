package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func newErrorHandlingRuleLogService(t *testing.T, settings *ErrorHandlingRuleSettings) *GatewayService {
	t.Helper()
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	cfg := &config.Config{}
	svc := &GatewayService{
		cfg:              cfg,
		settingService:   NewSettingService(repo, cfg),
		rateLimitService: &RateLimitService{},
		deferredService:  &DeferredService{},
	}
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), settings))
	return svc
}

// errorHandlingRuleLogContext 造一个只带 ctxkey、没有中间件塞 logger 的 ctx，
// 也就是转发链路脱离请求生命周期后的真实形态，走 FromContext 的兜底重建。
func errorHandlingRuleLogContext() context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-ehr")
	ctx = context.WithValue(ctx, ctxkey.RequestID, "req-ehr")
	return context.WithValue(ctx, ctxkey.ClientRequestID, "client-ehr")
}

// 触发日志必须能落到 logger.SetSink 这个出口：它既是容器 stdout（Vector →
// OpenObserve）那条链路的同一份数据，也是 OpsSystemLogSink 入库 ops_system_logs
// 的入口。级别必须是 warn 及以上——OpsSystemLogSink.shouldIndex 只收 warn+，
// 而生产把 log.level 调到 warn 时 info 行会直接被 zap 丢掉，这两种情况下
// 「引擎到底命中了什么」都会查不到。
func TestApplyErrorHandlingRuleLogReachesLogSinkWithCorrelationFields(t *testing.T) {
	sink := captureGatewayLogEvents(t)
	svc := newErrorHandlingRuleLogService(t, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{
			{ID: "r1", Name: "proxy failure", StatusCodes: []int{500}, Action: ErrorHandlingActionFailover},
		},
	})

	ctx := errorHandlingRuleLogContext()
	account := newErrorHandlingRuleForwardAccount()
	resp := &http.Response{
		StatusCode: 500,
		Header:     http.Header{"X-Request-Id": []string{"upstream-req-1"}},
		Body:       http.NoBody,
	}
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	var tracker errorHandlingRuleTracker
	outcome, _, err := svc.applyErrorHandlingRule(ctx, c, &tracker, account, resp,
		[]byte(`{"error":{"message":"proxy failure"}}`), "claude-sonnet-4-5", 1, time.Now(), false)
	require.Equal(t, errorHandlingRuleOutcomeDone, outcome)
	require.Error(t, err)

	event := sink.find(t, "error_handling_rule_matched")
	require.Equal(t, "warn", event.Level, "info 级别在 log.level=warn 的部署里会被丢掉，也进不了 ops_system_logs")
	require.Equal(t, componentGateway, event.Fields["component"])
	require.Equal(t, "trace-ehr", event.Fields["trace_id"])
	require.Equal(t, "req-ehr", event.Fields["request_id"])
	require.Equal(t, "client-ehr", event.Fields["client_request_id"])

	require.Equal(t, "r1", event.Fields["rule_id"])
	require.Equal(t, "proxy failure", event.Fields["rule_name"])
	require.Equal(t, ErrorHandlingActionFailover, event.Fields["rule_action"])
	require.Equal(t, ErrorHandlingActionFailover, event.Fields["outcome"])
	require.EqualValues(t, 500, event.Fields["upstream_status_code"])
	require.Equal(t, "upstream-req-1", event.Fields["upstream_request_id"])
	require.EqualValues(t, account.ID, event.Fields["account_id"])
}

// retry 动作每消耗一次重试预算都要留痕，否则调优时看不出规则是重试到耗尽换的号
// 还是一命中就换号。
func TestApplyErrorHandlingRuleLogsRetryBudgetUsage(t *testing.T) {
	sink := captureGatewayLogEvents(t)
	svc := newErrorHandlingRuleLogService(t, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{
			{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(2)},
		},
	})

	resp := &http.Response{StatusCode: 500, Header: http.Header{}, Body: http.NoBody}
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	var tracker errorHandlingRuleTracker
	outcome, _, err := svc.applyErrorHandlingRule(errorHandlingRuleLogContext(), c, &tracker,
		newErrorHandlingRuleForwardAccount(), resp, []byte(`{"error":{"message":"boom"}}`),
		"claude-sonnet-4-5", 1, time.Now(), false)
	require.NoError(t, err)
	require.Equal(t, errorHandlingRuleOutcomeRetry, outcome)

	event := sink.find(t, "error_handling_rule_matched")
	require.Equal(t, ErrorHandlingActionRetry, event.Fields["rule_action"])
	require.Equal(t, ErrorHandlingActionRetry, event.Fields["outcome"])
	require.EqualValues(t, 1, event.Fields["rule_retry_used"])
	require.EqualValues(t, 2, event.Fields["rule_retry_limit"])
}

// 重试预算用尽后降级换号：配的动作和实际动作不一样，必须一眼能看出降级原因。
func TestApplyErrorHandlingRuleLogsRetryDowngradeReason(t *testing.T) {
	sink := captureGatewayLogEvents(t)
	svc := newErrorHandlingRuleLogService(t, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{
			{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(0)},
		},
	})

	resp := &http.Response{StatusCode: 500, Header: http.Header{}, Body: http.NoBody}
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	var tracker errorHandlingRuleTracker
	_, _, err := svc.applyErrorHandlingRule(errorHandlingRuleLogContext(), c, &tracker,
		newErrorHandlingRuleForwardAccount(), resp, []byte(`{"error":{"message":"boom"}}`),
		"claude-sonnet-4-5", 1, time.Now(), false)
	require.Error(t, err)

	event := sink.find(t, "error_handling_rule_matched")
	require.Equal(t, ErrorHandlingActionRetry, event.Fields["rule_action"])
	require.Equal(t, ErrorHandlingActionFailover, event.Fields["outcome"])
	require.Equal(t, "retry_budget_exhausted", event.Fields["downgrade_reason"])
}
