package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
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

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "stream read error")

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
}

func TestPassthroughIntervalTimeoutRecordsOpsCause(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	upstream := &singleResponseHTTPUpstream{status: 200, body: &blockingBody{release: release}}
	svc := newStreamFailureUpstreamService(t, upstream, 1) // 1s：测试里能接受的最小间隔
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "stream data interval timeout")

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, opsUpstreamErrorKindStreamFailure, events[0].Kind)
	require.Equal(t, string(streamFailureIntervalTimeout), events[0].Reason)
	require.Equal(t, streamFailureScopeBeforeFirstToken, events[0].Scope)
	require.True(t, events[0].Passthrough)
	// upstream_status_code 必须保持不写：wire 上游状态确实是 200。
	require.Zero(t, events[0].UpstreamStatusCode)
	require.Equal(t, "stream data interval timeout", events[0].Message)
}

func TestPassthroughUnmatchedStreamErrorIsRecordedOnce(t *testing.T) {
	body := "event: error\ndata: " + streamRuleConcurrencyError + "\n\n" +
		"event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	// 规则限定 502，第一级用的是 429（rate_limit_error），必然未命中。
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
			require.Contains(t, ev.Message, "Concurrency limit exceeded")
		}
	}
	require.Equal(t, 1, unmatched, "同一 attempt 内连续多个未命中只记一条")
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
		svc.recordStreamFailureCause(context.Background(), nil, nil, "m", streamFailureReadError, "boom", false)
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	require.NotPanics(t, func() {
		svc.recordStreamFailureCause(context.Background(), c, nil, "m", streamFailureReadError, "boom", false)
	})
	_, ok := c.Get(OpsUpstreamErrorsKey)
	require.False(t, ok, "account 缺失时不应写入 ops")
}
