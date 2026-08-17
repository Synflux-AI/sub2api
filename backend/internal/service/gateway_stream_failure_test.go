package service

import (
	"context"
	"testing"

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
