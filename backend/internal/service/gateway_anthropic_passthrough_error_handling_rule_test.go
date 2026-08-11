package service

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const (
	streamRuleConcurrencyError = `{"type":"error","error":{"type":"rate_limit_error","message":"Concurrency limit exceeded for account, please retry later"}}`
	streamRuleSuccess          = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_ok","model":"claude-sonnet-4-5","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
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
		settingService: NewSettingService(repo, cfg), deferredService: &DeferredService{},
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), ruleSettings))
	return svc
}

func newErrorHandlingRuleStreamParsed(t *testing.T) *ParsedRequest {
	t.Helper()
	body := []byte(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}

func streamErrorRule(action string, retries int, exhaustedAction string) ErrorHandlingRule {
	return ErrorHandlingRule{
		ID: "stream-concurrency", StatusCodes: []int{http.StatusTooManyRequests},
		Keywords: []string{"Concurrency limit exceeded"}, Action: action,
		RetryCount: errorHandlingIntPtr(retries), ExhaustedAction: exhaustedAction,
	}
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

func TestPassthroughStreamErrorRuleRetriesInPlaceThenSucceeds(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: "event: error\ndata: " + streamRuleConcurrencyError + "\n\n"},
		{status: 200, body: streamRuleSuccess},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionRetry, 1, ErrorHandlingExhaustedActionDefault)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	result, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.calls)
	require.NotContains(t, recorder.Body.String(), streamRuleConcurrencyError)
	require.Contains(t, recorder.Body.String(), "event: message_stop")
}

func TestPassthroughStreamErrorRuleRetryExhaustionReturnsTypedFailover(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{
		status: 200, body: "event: error\ndata: " + streamRuleConcurrencyError + "\n\n",
	}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionRetry, 1, ErrorHandlingExhaustedActionPassthrough)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 2, upstream.calls)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Equal(t, ErrorHandlingExhaustedActionPassthrough, failoverErr.ExhaustedAction)
	require.Equal(t, "rate_limit_error", failoverErr.SafeErrorType)
	require.Contains(t, failoverErr.SafeErrorMessage, "Concurrency limit exceeded")
	require.Empty(t, recorder.Body.String(), "held error events must not reach the client before failover")
}

func TestPassthroughStreamErrorRuleFailsOverImmediately(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{
		status: 200, body: "data: " + streamRuleConcurrencyError + "\n\n",
	}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionFailover, 0, ErrorHandlingExhaustedActionDefault)},
	})
	_, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 1, upstream.calls)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
}

func TestPassthroughStreamRuleMatchesMultiDataEventWithoutTrailingBlankLine(t *testing.T) {
	body := "event: error\n" +
		`data: {"type":"error",` + "\n" +
		`data: "error":{"type":"rate_limit_error","message":"Concurrency limit exceeded for account"}}`
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionFailover, 0, ErrorHandlingExhaustedActionDefault)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.Empty(t, recorder.Body.String())
}

func TestPassthroughStreamErrorRuleReturnsCompleteEventOnce(t *testing.T) {
	rawEvent := "event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: rawEvent}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionPassthrough, 0, ErrorHandlingExhaustedActionDefault)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.Error(t, err)
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, rawEvent, recorder.Body.String())
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
	require.NotContains(t, recorder.Body.String(), "Upstream request failed")
}

func TestPassthroughStreamStandardErrorEventWithInvalidJSONIsHeldAndReturnedOnce(t *testing.T) {
	rawEvent := "event: error\ndata: invalid-upstream-payload\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: rawEvent}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{{
			ID: "invalid-json", StatusCodes: []int{http.StatusInternalServerError}, Keywords: []string{"invalid-upstream-payload"},
			Action: ErrorHandlingActionPassthrough, ExhaustedAction: ErrorHandlingExhaustedActionDefault,
		}},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.Error(t, err)
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, rawEvent, recorder.Body.String())
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
}

func TestAnthropicPassthroughSSECommentIsNotSemantic(t *testing.T) {
	event := buildAnthropicPassthroughSSEEvent([]string{" : upstream comment"}, true)
	require.False(t, anthropicPassthroughSSEEventIsSemantic(event))
}

func TestPassthroughStreamUnmatchedErrorPreservesLegacyFallbackContract(t *testing.T) {
	rawEvent := "event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: rawEvent}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "other", StatusCodes: []int{502}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)}},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.ErrorContains(t, err, "missing terminal event")
	require.False(t, IsResponseCommitted(c))
	require.Equal(t, rawEvent, recorder.Body.String())
}

func TestPassthroughStreamRuleDowngradesFailoverAfterSemanticOutput(t *testing.T) {
	body := strings.TrimSuffix(streamRuleSuccess, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") +
		"event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionFailover, 0, ErrorHandlingExhaustedActionDefault)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, 1, upstream.calls)
	require.Contains(t, recorder.Body.String(), "event: message_start")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
}

func TestPassthroughStreamSemanticStateIsStickyAcrossAttempts(t *testing.T) {
	svc := newErrorHandlingRulePassthroughService(t, &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200}}}, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionRetry, 1, ErrorHandlingExhaustedActionDefault)},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	account := newErrorHandlingRulePassthroughAccount()
	var tracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState

	firstResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(streamRuleSuccess)),
	}
	_, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), firstResp, c, account, time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
	)
	require.NoError(t, err)
	require.Nil(t, match)
	require.True(t, streamState.semanticEventForwarded)

	secondResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("event: error\ndata: " + streamRuleConcurrencyError + "\n\n")),
	}
	_, match, err = svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), secondResp, c, account, time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 2,
	)
	require.NoError(t, err)
	require.NotNil(t, match)
	require.True(t, match.semanticEventForwarded)
	require.Equal(t, ErrorHandlingActionPassthrough, match.decision.EffectiveAction)
	require.Equal(t, "semantic_output_started", match.decision.DowngradeReason)
}

func TestPassthroughStreamRuleAllowsFailoverAfterUpstreamPing(t *testing.T) {
	body := "event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionFailover, 0, ErrorHandlingExhaustedActionDefault)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Contains(t, recorder.Body.String(), "event: ping")
	require.NotContains(t, recorder.Body.String(), "event: error")
}

func TestPassthroughStreamCleanEOFRetriesThroughVirtual502(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: ""},
		{status: 200, body: streamRuleSuccess},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{{
			ID: "early-eof", StatusCodes: []int{http.StatusBadGateway}, Keywords: []string{"missing terminal event"},
			Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1), ExhaustedAction: ErrorHandlingExhaustedActionDefault,
		}},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	result, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.calls)
	require.NotContains(t, recorder.Body.String(), "stream_error")
}

func TestPassthroughStreamCleanEOFAfterLocalKeepaliveCanFailOver(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: ""}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{{
			ID: "early-eof", StatusCodes: []int{http.StatusBadGateway}, Keywords: []string{"missing terminal event"},
			Action: ErrorHandlingActionFailover, ExhaustedAction: ErrorHandlingExhaustedActionDefault,
		}},
	})
	svc.cfg.Gateway.StreamKeepaliveInterval = 1

	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(1100 * time.Millisecond)
		_ = writer.Close()
	}()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}
	var tracker errorHandlingRuleTracker
	result, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(), time.Now(), "claude-sonnet-4-5", &tracker, nil, 1,
	)
	_ = reader.Close()
	<-done

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, match)
	require.True(t, match.synthetic)
	require.False(t, match.semanticEventForwarded)
	require.Equal(t, ErrorHandlingActionFailover, match.decision.EffectiveAction)
	require.Contains(t, recorder.Body.String(), "event: ping")
	require.NotContains(t, recorder.Body.String(), "event: error")
}

func TestPassthroughStreamCleanEOFDoesNotRetryAfterSemanticOutput(t *testing.T) {
	body := strings.Split(streamRuleSuccess, "event: message_stop")[0]
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules: []ErrorHandlingRule{{
			ID: "early-eof", StatusCodes: []int{http.StatusBadGateway}, Keywords: []string{"missing terminal event"},
			Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1), ExhaustedAction: ErrorHandlingExhaustedActionDefault,
		}},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.ErrorContains(t, err, "missing terminal event")
	require.Equal(t, 1, upstream.calls)
	require.Contains(t, recorder.Body.String(), "event: message_start")
}

func TestPassthroughStreamEventBufferIsBoundedAcrossLines(t *testing.T) {
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 128}}}
	line := ":" + strings.Repeat("x", 100) + "\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(strings.Repeat(line, 50))),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(), time.Now(), "claude-sonnet-4-5",
	)
	require.ErrorIs(t, err, bufio.ErrTooLong)
	require.NotNil(t, result)
	require.Empty(t, recorder.Body.String())
}

func TestStreamRuleRetryIgnoresHTTPRetryElapsedWindow(t *testing.T) {
	svc := newErrorHandlingRuleService(t, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionRetry, 1, ErrorHandlingExhaustedActionDefault)},
	})
	var tracker errorHandlingRuleTracker
	decision := svc.decideErrorHandlingRule(context.Background(), &tracker, newErrorHandlingRulePassthroughAccount(), http.StatusTooManyRequests,
		[]byte(streamRuleConcurrencyError), "claude-sonnet-4-5", errorHandlingRuleDecisionOptions{
			Attempt: 1, RetryStart: time.Now().Add(-30 * time.Second), IgnoreRetryElapsed: true, IndependentRetryBudget: true,
		})
	require.True(t, decision.Matched)
	require.Equal(t, ErrorHandlingActionRetry, decision.EffectiveAction)
	require.Empty(t, decision.DowngradeReason)
}
