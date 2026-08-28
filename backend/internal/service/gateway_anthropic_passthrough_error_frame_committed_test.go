package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// #201：多级代理串联时，中间那一跳把上游 error 帧原样转发给客户端后，若流随即
// 结束/超时，handler 的 ensureForwardErrorResponse 会再补一条同样的终止 error 帧，
// 客户端因此收到两条。修复口径：透传路径只要真正把 error 帧写给了客户端，就要
// MarkResponseCommitted，让 handler 的兜底短路——与命中规则的 passthrough 分支对齐。
const passthroughUpstreamErrorFrame = "event: error\n" +
	`data: {"error":{"message":"Upstream request failed","type":"upstream_error"},"type":"error"}` + "\n\n"

func newPassthroughErrorFrameService(t *testing.T, gatewayCfg config.GatewayConfig) *GatewayService {
	t.Helper()
	cfg := &config.Config{Gateway: gatewayCfg}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		settingService:       NewSettingService(&gatewayTTLSettingRepo{data: map[string]string{}}, cfg),
		deferredService:      &DeferredService{},
	}
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), &ErrorHandlingRuleSettings{}))
	return svc
}

// 上游 200 流内发 error 帧、规则未命中、流无终止事件：客户端必须恰好收到 1 条 error 帧，
// 且 ResponseCommitted 已置位，handler 不会再补第二条。
func TestPassthroughUnmatchedErrorFrameMarksCommittedOnMissingTerminal(t *testing.T) {
	svc := newPassthroughErrorFrameService(t, config.GatewayConfig{MaxLineSize: defaultMaxLineSize})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(passthroughUpstreamErrorFrame)),
	}
	var tracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState
	result, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(),
		time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
	)

	require.Nil(t, match)
	require.NotNil(t, result)
	require.ErrorContains(t, err, "missing terminal event")
	require.True(t, IsResponseCommitted(c), "已转发的 error 帧就是对客终止错误，handler 兜底必须短路")
	require.Equal(t, passthroughUpstreamErrorFrame, recorder.Body.String())
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
}

// 同一缺口的另一条 return 路径：error 帧转发后上游静默到间隔超时。
func TestPassthroughUnmatchedErrorFrameMarksCommittedOnIntervalTimeout(t *testing.T) {
	svc := newPassthroughErrorFrameService(t, config.GatewayConfig{
		MaxLineSize:               defaultMaxLineSize,
		StreamDataIntervalTimeout: 1,
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(passthroughUpstreamErrorFrame))
	}()
	defer func() {
		_ = pw.Close()
		_ = pr.Close()
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}
	var tracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState
	_, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(),
		time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
	)

	require.Nil(t, match)
	require.ErrorContains(t, err, "stream data interval timeout")
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
}

// 反向守卫一：从未转发过 error 帧的纯 read_error 不得置位，否则客户端会拿到 silent EOF。
func TestPassthroughReadErrorWithoutErrorFrameKeepsFallback(t *testing.T) {
	svc := newPassthroughErrorFrameService(t, config.GatewayConfig{MaxLineSize: defaultMaxLineSize})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	payload := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &streamReadCloser{payload: []byte(payload), err: io.ErrUnexpectedEOF},
	}
	var tracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState
	_, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(),
		time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
	)

	require.Nil(t, match)
	require.ErrorContains(t, err, "stream read error")
	require.False(t, IsResponseCommitted(c), "没给客户端发过终止错误时兜底必须照旧补写")
	require.NotContains(t, recorder.Body.String(), "event: error")
}

// 反向守卫二：客户端已断开时 writeEvent 写失败，帧根本没送达，标记不能误置。
func TestPassthroughErrorFrameWriteFailureDoesNotMarkCommitted(t *testing.T) {
	svc := newPassthroughErrorFrameService(t, config.GatewayConfig{MaxLineSize: defaultMaxLineSize})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(passthroughUpstreamErrorFrame)),
	}
	var tracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState
	result, _, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(),
		time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
	)

	require.Error(t, err)
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
	require.False(t, IsResponseCommitted(c), "写失败的 error 帧没有交付，不构成已交付终止错误")
}

// 转发过 error 帧但流仍以 message_stop 正常收尾时不返回错误，handler 本就不会兜底；
// 这条锁住「提前置位」不会顺带改变正常收尾路径的 wire 输出。
func TestPassthroughErrorFrameFollowedByTerminalEventStillWritesOnce(t *testing.T) {
	svc := newPassthroughErrorFrameService(t, config.GatewayConfig{MaxLineSize: defaultMaxLineSize})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(passthroughUpstreamErrorFrame + "event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n")),
	}
	var tracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState
	_, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(),
		time.Now(), "claude-sonnet-4-5", &tracker, &streamState, 1,
	)

	require.Nil(t, match)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
	require.Contains(t, recorder.Body.String(), "event: message_stop")
}
