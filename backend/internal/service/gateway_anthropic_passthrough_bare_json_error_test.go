package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #190：上游在 200 流里写「没有 data: 前缀的裸 JSON 错误体」时，错误处理规则被双重绕过。
const bareJSONUpstreamError = `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.cache_control.ttl: a ttl='1h' cache_control block must not come after a ttl='5m' cache_control block."},"request_id":"req_011CeRa5MGr11g8kov8HepBK"}`

func bareJSONErrorRule(action string) ErrorHandlingRule {
	return ErrorHandlingRule{
		ID: "bare-json-client-error", StatusCodes: []int{},
		Keywords: []string{"cache_control block must not"}, Action: action,
		ExhaustedAction: ErrorHandlingExhaustedActionDefault,
	}
}

func missingTerminalRetryRule() ErrorHandlingRule {
	return ErrorHandlingRule{
		ID: "early-eof", StatusCodes: []int{http.StatusBadGateway}, Keywords: []string{"missing terminal event"},
		Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1), ExhaustedAction: ErrorHandlingExhaustedActionDefault,
	}
}

// 方案 2：整行合法 JSON 且 type=error 的裸行必须被识别成 error 事件，规则引擎才拿得到上游原文。
func TestAnthropicPassthroughBareJSONErrorLineIsClassifiedAsError(t *testing.T) {
	event := buildAnthropicPassthroughSSEEvent([]string{bareJSONUpstreamError}, true)

	statusCode, errType, message, isError := anthropicPassthroughSSEError(event)
	require.True(t, isError, "裸 JSON 错误体必须进 error 分支，否则一级匹配根本不会被调用")
	require.Equal(t, http.StatusBadRequest, statusCode)
	require.Equal(t, "invalid_request_error", errType)
	require.Contains(t, message, "cache_control block must not")
}

// 方案 1：裸行不承载语义输出，不能置 semanticEventForwarded。
func TestAnthropicPassthroughBareJSONErrorLineIsNotSemantic(t *testing.T) {
	event := buildAnthropicPassthroughSSEEvent([]string{bareJSONUpstreamError}, true)
	require.False(t, anthropicPassthroughSSEEventIsSemantic(event))
}

// 方案 2 的填充不得反向顶开方案 1 的守卫：空 error 对象过不了 errorObjectNonEmpty，
// 若 semantic 判定挂在 len(event.data) 上，这条会原样复现 #190。
func TestAnthropicPassthroughBareJSONEmptyErrorObjectIsNeitherErrorNorSemantic(t *testing.T) {
	event := buildAnthropicPassthroughSSEEvent([]string{`{"type":"error","error":{}}`}, true)

	_, _, _, isError := anthropicPassthroughSSEError(event)
	require.False(t, isError)
	require.False(t, anthropicPassthroughSSEEventIsSemantic(event), "没有 data: 行的事件一律不算语义输出")
}

// 非 type=error 的裸行不在方案 2 的收窄条件内，只受方案 1 影响。
func TestAnthropicPassthroughBareNonErrorJSONLineIsNotSemantic(t *testing.T) {
	for _, line := range []string{
		`{"type":"content_block_delta","delta":{"text":"hi"}}`,
		`not-json-at-all`,
		`[{"type":"error"}]`,
	} {
		event := buildAnthropicPassthroughSSEEvent([]string{line}, true)
		_, _, _, isError := anthropicPassthroughSSEError(event)
		require.False(t, isError, "line=%s", line)
		require.False(t, anthropicPassthroughSSEEventIsSemantic(event), "line=%s", line)
	}
}

// data: [DONE] 由 terminal 判定单独处理，算 semantic 会破坏下面的不变式。
func TestAnthropicPassthroughDoneDataLineIsNotSemantic(t *testing.T) {
	event := buildAnthropicPassthroughSSEEvent([]string{"data: [DONE]"}, true)
	require.False(t, anthropicPassthroughSSEEventIsSemantic(event))
}

// 空 data 行同样不承载输出，且必须与 processEvent 里 TrimSpace(data) != "" 的谓词保持一致。
func TestAnthropicPassthroughEmptyDataLineIsNotSemantic(t *testing.T) {
	event := buildAnthropicPassthroughSSEEvent([]string{"data:"}, true)
	require.False(t, anthropicPassthroughSSEEventIsSemantic(event))
}

func newBareJSONStreamResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 不变式：semanticEventForwarded == true ⇒ firstTokenMs != nil。
// #190 的生产记录里 scope=before_first_token 与 semanticEventForwarded=true 同时成立，自相矛盾。
func TestPassthroughStreamSemanticForwardedImpliesFirstToken(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"裸 JSON 错误行", bareJSONUpstreamError + "\n\n"},
		{"裸 JSON 空 error 对象", `{"type":"error","error":{}}` + "\n\n"},
		{"裸非 JSON 行", "garbage-line\n\n"},
		{"只有注释", ": upstream comment\n\n"},
		{"只有 ping", "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{"空 data 行", "data:\n\n"},
		{"[DONE]", "data: [DONE]\n\n"},
		{"不带 data 的 message_stop", "event: message_stop\n\n"},
		{"正常语义输出", streamRuleSuccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newErrorHandlingRulePassthroughService(t, &sequencedHTTPUpstream{}, &ErrorHandlingRuleSettings{})
			c, _ := newErrorHandlingRuleTestContextWithRecorder()
			var tracker errorHandlingRuleTracker
			var streamState anthropicPassthroughStreamState
			result, _, _ := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
				context.Background(), newBareJSONStreamResponse(tc.body), c, newErrorHandlingRulePassthroughAccount(),
				time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
			)
			require.NotNil(t, result)
			if streamState.semanticEventForwarded {
				require.NotNil(t, result.firstTokenMs, "置了 semanticEventForwarded 就必须已推进 firstTokenMs")
			}
		})
	}
}

// 不带 data 的 event: message_stop 仍要置 sawTerminalEvent（不能被本次改动波及）。
func TestPassthroughStreamTerminalEventWithoutDataStaysTerminal(t *testing.T) {
	svc := newErrorHandlingRulePassthroughService(t, &sequencedHTTPUpstream{}, &ErrorHandlingRuleSettings{})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	var tracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState

	result, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), newBareJSONStreamResponse("event: message_stop\n\n"), c, newErrorHandlingRulePassthroughAccount(),
		time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
	)

	require.NoError(t, err, "terminal 事件在场就不该报 missing terminal event")
	require.Nil(t, match)
	require.NotNil(t, result)
	require.False(t, streamState.semanticEventForwarded)
	require.Equal(t, "event: message_stop\n\n", recorder.Body.String(), "对客字节流逐字节不变")
}

// 方案 2 主收益：裸行的上游原文喂进引擎，「客户端错误」类规则一级命中直接换号。
func TestPassthroughStreamBareJSONErrorMatchesRuleAndFailsOver(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: bareJSONUpstreamError + "\n\n"},
		{status: 200, body: streamRuleSuccess},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{bareJSONErrorRule(ErrorHandlingActionFailover)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadRequest, failoverErr.StatusCode)
	require.Contains(t, failoverErr.SafeErrorMessage, "cache_control block must not")
	require.Equal(t, 1, upstream.calls)
	require.Empty(t, recorder.Body.String(), "一级命中即 return，裸行不该发给客户端")
}

// 方案 2 在同一路径上也让 retry 生效：换 attempt 后拿到正常流。
func TestPassthroughStreamBareJSONErrorRuleRetriesInPlaceThenSucceeds(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: bareJSONUpstreamError + "\n\n"},
		{status: 200, body: streamRuleSuccess},
	}}
	rule := bareJSONErrorRule(ErrorHandlingActionRetry)
	rule.RetryCount = errorHandlingIntPtr(1)
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{rule},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.calls)
	require.NotContains(t, recorder.Body.String(), "cache_control block must not")
	require.Contains(t, recorder.Body.String(), "event: message_stop")
}

// 方案 1 的独立收益：裸行不是合法的 type=error JSON（方案 2 够不着）时，
// 不再被误置 semantic，合成兜底照常触发，「流中断」规则救回。
func TestPassthroughStreamBareNonErrorLineStillReachesMissingTerminalRule(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: `{"type":"error","error":{}}` + "\n\n"},
		{status: 200, body: streamRuleSuccess},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{missingTerminalRetryRule()},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.calls, "零输出的裸行必须能干净重试")
	require.Contains(t, recorder.Body.String(), "event: message_stop")
}

// 方案 2 的取舍（PR 里已写明）：裸 JSON error 一旦被识别成 error 事件，
// sawAnyErrorEvent 就无条件置位；关键字没配上时不再走合成兜底，沿用 :777-779 的 legacy fallback 契约。
func TestPassthroughStreamUnmatchedBareJSONErrorKeepsLegacyFallbackContract(t *testing.T) {
	rawEvent := bareJSONUpstreamError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: rawEvent}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true, Rules: []ErrorHandlingRule{missingTerminalRetryRule()},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.ErrorContains(t, err, "missing terminal event")
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, rawEvent, recorder.Body.String(), "未命中时裸行照旧原样转发，对客字节流不变")
}

// 方案 2 合成出来的 data 会流进 usage/model 观测，必须无害。
func TestPassthroughStreamBareJSONErrorDoesNotPolluteUsage(t *testing.T) {
	svc := newErrorHandlingRulePassthroughService(t, &sequencedHTTPUpstream{}, &ErrorHandlingRuleSettings{})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	var tracker errorHandlingRuleTracker

	result, _, _ := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), newBareJSONStreamResponse(bareJSONUpstreamError+"\n\n"), c,
		newErrorHandlingRulePassthroughAccount(), time.Now(), "claude-sonnet-4-5", &tracker, nil, 1,
	)

	require.NotNil(t, result)
	require.Nil(t, result.firstTokenMs)
	require.Equal(t, 0, result.usage.InputTokens)
	require.Equal(t, 0, result.usage.OutputTokens)

	observer := upstreamResponseModelObserverFromContext(c)
	require.NotNil(t, observer)
	require.Empty(t, observer.Model(), "错误体不该被当成上游响应模型")
	require.False(t, observer.Conflict())
}
