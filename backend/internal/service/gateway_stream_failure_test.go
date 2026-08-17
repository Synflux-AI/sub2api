package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// newStreamFailureLogObserver 装一个 zaptest/observer core，通过
// logger.IntoContext 注入 ctx，供验收 1（三个终止点各产生一条
// gateway.stream_failure 结构化 warn，带 cause/scope/account_id/model）用
// 观测方式断言，而不是走「日志到底打没打」的猜测。
// GatewayService.Forward 直接把调用方传入的 ctx 转发到
// recordStreamFailureCause，中途不重新绑定 request context，因此这里可以
// 直接把观测用的 ctx 传给 svc.Forward。
func newStreamFailureLogObserver(t *testing.T) (context.Context, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	return logger.IntoContext(context.Background(), zap.New(core)), logs
}

// requireStreamFailureLog 断言 observedLogs 里恰好有一条 gateway.stream_failure
// warn，并校验验收 1 要求的字段名与取值：cause / scope / account_id / model。
func requireStreamFailureLog(t *testing.T, logs *observer.ObservedLogs, wantCause streamFailureCause, wantScope string, wantAccountID int64, wantModel string) {
	t.Helper()
	entries := logs.FilterMessage("gateway.stream_failure").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, string(wantCause), fields["cause"])
	require.Equal(t, wantScope, fields["scope"])
	require.EqualValues(t, wantAccountID, fields["account_id"])
	require.Equal(t, wantModel, fields["model"])
}

func opsUpstreamEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "expected ops upstream errors on context")
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

func TestPassthroughMissingTerminalRecordsOpsCause(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: ""}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{Enabled: false})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	ctx, logs := newStreamFailureLogObserver(t)

	_, err := svc.Forward(ctx, c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "missing terminal event")

	msg, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok)
	require.Equal(t, "stream usage incomplete: missing terminal event", msg)

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, opsUpstreamErrorKindStreamFailure, events[0].Kind)
	require.Equal(t, string(streamFailureMissingTerminal), events[0].Reason)
	require.Equal(t, streamFailureScopeBeforeFirstToken, events[0].Scope)
	require.True(t, events[0].Passthrough)
	// upstream_status_code 必须保持不写：wire 上游状态确实是 200，
	// 合成一个 5xx 会污染上游状态维度。
	require.Zero(t, events[0].UpstreamStatusCode)

	// 验收 1：三个终止点各自产生一条带 cause/scope/account_id/model 的
	// gateway.stream_failure 结构化 warn。account 702、model 见
	// newErrorHandlingRulePassthroughAccount / newErrorHandlingRuleStreamParsed。
	requireStreamFailureLog(t, logs, streamFailureMissingTerminal, streamFailureScopeBeforeFirstToken, 702, "claude-sonnet-4-5")
}

func TestPassthroughMissingTerminalAfterFirstTokenRecordsScope(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_x","model":"claude-sonnet-4-5","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{Enabled: false})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "missing terminal event")

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, streamFailureScopeAfterFirstToken, events[0].Scope)
}

// erroringBody is an io.ReadCloser whose Read always fails with a fixed,
// non-EOF error. It simulates a mid-stream network failure (e.g. connection
// reset) that sequencedHTTPUpstream's string-only body cannot express, so we
// can exercise the read_error termination point end-to-end through Forward.
type erroringBody struct {
	err error
}

func (b *erroringBody) Read(_ []byte) (int, error) { return 0, b.err }
func (b *erroringBody) Close() error               { return nil }

// blockingBody is an io.ReadCloser whose Read blocks until release is closed.
// It simulates a stalled upstream that never sends another byte, so the
// interval_timeout termination point can be exercised without racing against
// an accidental EOF/error path. Tests must close release before returning to
// unblock the background scanner goroutine and avoid leaking it.
type blockingBody struct {
	release chan struct{}
}

func (b *blockingBody) Read(_ []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}
func (b *blockingBody) Close() error { return nil }

// singleResponseHTTPUpstream returns one fixed *http.Response, with a
// caller-supplied body, for every call. Used where sequencedHTTPUpstream's
// string-backed body can't express a body that errors or blocks mid-read.
type singleResponseHTTPUpstream struct {
	status int
	body   io.ReadCloser
}

func (u *singleResponseHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       u.body,
	}, nil
}

func (u *singleResponseHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

// newStreamFailureUpstreamService mirrors newErrorHandlingRulePassthroughService
// but allows an arbitrary HTTPUpstream (needed for bodies that error/block
// mid-read) and a configurable stream data interval timeout.
func newStreamFailureUpstreamService(t *testing.T, upstream HTTPUpstream, streamDataIntervalTimeout int) *GatewayService {
	t.Helper()
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	cfg := &config.Config{Gateway: config.GatewayConfig{
		MaxLineSize:               defaultMaxLineSize,
		StreamDataIntervalTimeout: streamDataIntervalTimeout,
	}}
	svc := &GatewayService{
		cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream,
		settingService: NewSettingService(repo, cfg), deferredService: &DeferredService{},
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), &ErrorHandlingRuleSettings{Enabled: false}))
	return svc
}

func TestPassthroughReadErrorRecordsOpsCause(t *testing.T) {
	readErr := errors.New("connection reset by peer")
	upstream := &singleResponseHTTPUpstream{status: 200, body: &erroringBody{err: readErr}}
	svc := newStreamFailureUpstreamService(t, upstream, 0)
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	ctx, logs := newStreamFailureLogObserver(t)

	_, err := svc.Forward(ctx, c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "stream read error")

	// 验收 2：三个 cause 都写 upstream_error_message，不止 missing_terminal
	// 那一个用例断言过。
	msg, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok)
	msgStr, ok := msg.(string)
	require.True(t, ok, "message=%v", msg)
	require.True(t, strings.HasPrefix(msgStr, "stream read error:"), "message=%q", msgStr)

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, opsUpstreamErrorKindStreamFailure, events[0].Kind)
	require.Equal(t, string(streamFailureReadError), events[0].Reason)
	require.Equal(t, streamFailureScopeBeforeFirstToken, events[0].Scope)
	require.True(t, events[0].Passthrough)
	// upstream_status_code 必须保持不写：wire 上游状态确实是 200。
	require.Zero(t, events[0].UpstreamStatusCode)
	// 尾部是动态 error 文本（readErr.Error()），只断言前缀。
	require.True(t, strings.HasPrefix(events[0].Message, "stream read error:"), "message=%q", events[0].Message)

	requireStreamFailureLog(t, logs, streamFailureReadError, streamFailureScopeBeforeFirstToken, 702, "claude-sonnet-4-5")
}

func TestPassthroughIntervalTimeoutRecordsOpsCause(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	upstream := &singleResponseHTTPUpstream{status: 200, body: &blockingBody{release: release}}
	svc := newStreamFailureUpstreamService(t, upstream, 1) // 1s：测试里能接受的最小间隔
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	ctx, logs := newStreamFailureLogObserver(t)

	_, err := svc.Forward(ctx, c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "stream data interval timeout")

	// 验收 2：三个 cause 都写 upstream_error_message。
	msg, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok)
	require.Equal(t, "stream data interval timeout", msg)

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, opsUpstreamErrorKindStreamFailure, events[0].Kind)
	require.Equal(t, string(streamFailureIntervalTimeout), events[0].Reason)
	require.Equal(t, streamFailureScopeBeforeFirstToken, events[0].Scope)
	require.True(t, events[0].Passthrough)
	// upstream_status_code 必须保持不写：wire 上游状态确实是 200。
	require.Zero(t, events[0].UpstreamStatusCode)
	require.Equal(t, "stream data interval timeout", events[0].Message)

	requireStreamFailureLog(t, logs, streamFailureIntervalTimeout, streamFailureScopeBeforeFirstToken, 702, "claude-sonnet-4-5")
}

func TestPassthroughUnmatchedStreamErrorIsRecordedOnce(t *testing.T) {
	// 两条未命中 payload 内容不同（分别是 api_error / rate_limit_error），
	// 这样断言「记录的是第一条」才能真正区分「保留首条」「保留末条」
	// 「不去重、每次都覆盖」这三种实现——如果两条内容相同，三种实现都会
	// 产出同样的结果，测不出去重契约。
	firstUnmatched := `{"type":"error","error":{"type":"api_error","message":"first unmatched failure"}}`
	body := "event: error\ndata: " + firstUnmatched + "\n\n" +
		"event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	// 规则限定 502，两条 payload（500/429）都必然未命中。
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "other", StatusCodes: []int{502}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)}},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.Error(t, err)

	events := opsUpstreamEvents(t, c)
	unmatched := 0
	for _, ev := range events {
		if ev.Kind == opsUpstreamErrorKindStreamErrorUnmatched {
			unmatched++
			require.Contains(t, ev.Message, "first unmatched failure")
			require.NotContains(t, ev.Message, "Concurrency limit exceeded")
		}
	}
	require.Equal(t, 1, unmatched, "同一 attempt 内连续多个未命中只记首条")
}

func TestPassthroughMatchedStreamErrorSuppressesUnmatchedRecord(t *testing.T) {
	body := "event: error\ndata: " + `{"type":"error","error":{"type":"api_error","message":"transient blip"}}` + "\n\n" +
		"event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: body},
		{status: 200, body: streamRuleSuccess},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionRetry, 1, ErrorHandlingExhaustedActionDefault)},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.NoError(t, err)

	if v, ok := c.Get(OpsUpstreamErrorsKey); ok {
		events, _ := v.([]*OpsUpstreamErrorEvent)
		for _, ev := range events {
			require.NotEqual(t, opsUpstreamErrorKindStreamErrorUnmatched, ev.Kind,
				"同一 attempt 内后续 error 命中规则时，未命中记录必须被抑制")
		}
	}
}

func TestPassthroughUnmatchedStreamErrorIsSanitized(t *testing.T) {
	payload := `{"type":"error","error":{"type":"api_error","message":"denied","api_key":"sk-live-abcdefgh"}}`
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: "event: error\ndata: " + payload + "\n\n"},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "other", StatusCodes: []int{502}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)}},
	})
	// 必须打开 LogUpstreamErrorBody，否则 detail 恒为空，这条用例会空跑通过
	// 而完全没有验证脱敏。
	svc.cfg.Gateway.LogUpstreamErrorBody = true
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.Error(t, err)

	events := opsUpstreamEvents(t, c)
	unmatchedSeen := false
	for _, ev := range events {
		require.NotContains(t, ev.Message, "sk-live-abcdefgh")
		require.NotContains(t, ev.Detail, "sk-live-abcdefgh")
		if ev.Kind == opsUpstreamErrorKindStreamErrorUnmatched {
			unmatchedSeen = true
			// detail 必须真的带上了原文（脱敏后），否则等于没测。
			require.Contains(t, ev.Detail, upstreamSensitiveMask)
			require.Contains(t, ev.Detail, "denied")
		}
	}
	require.True(t, unmatchedSeen)
}

func TestRecordStreamFailureCauseIsNilSafe(t *testing.T) {
	svc := &GatewayService{}
	require.NotPanics(t, func() {
		svc.recordStreamFailureCause(context.Background(), nil, nil, "m", streamFailureReadError, "boom", false, false)
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	require.NotPanics(t, func() {
		svc.recordStreamFailureCause(context.Background(), c, nil, "m", streamFailureReadError, "boom", false, false)
	})
	_, ok := c.Get(OpsUpstreamErrorsKey)
	require.False(t, ok, "account 缺失时不应写入 ops")
}

// TestRecordStreamFailureCauseTagsClientDisconnected 覆盖 Finding 5：
// missing_terminal 分支不因客户端已断开而提前 return（read_error /
// interval_timeout 会），会与上游真断流的样本混进同一个 cause 桶。这里直接
// 单测 recordStreamFailureCause 的打标行为——不依赖端到端复现
// clientDisconnected && streamInterval<=0 这个具体分支组合。
func TestRecordStreamFailureCauseTagsClientDisconnected(t *testing.T) {
	svc := &GatewayService{}
	account := newErrorHandlingRulePassthroughAccount()
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	ctx, logs := newStreamFailureLogObserver(t)

	svc.recordStreamFailureCause(ctx, c, account, "claude-sonnet-4-5", streamFailureMissingTerminal, "stream usage incomplete: missing terminal event", false, true)

	entries := logs.FilterMessage("gateway.stream_failure").All()
	require.Len(t, entries, 1)
	require.Equal(t, true, entries[0].ContextMap()["client_disconnected"])

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "client_disconnected", events[0].Stage)
}
