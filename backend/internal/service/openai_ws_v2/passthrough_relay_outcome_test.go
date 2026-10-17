package openai_ws_v2

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/responseoutcome"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 同一连接上的多个响应按 response_id 各自记录结束原因，不能互相串号，
// 也不能把连接关闭当作每个响应的结束原因。
func TestRelay_OnTurnComplete_ResponseOutcomePerResponse(t *testing.T) {
	t.Parallel()

	frames := []string{
		`{"type":"response.created","response":{"id":"resp_a","status":"in_progress"}}`,
		`{"type":"response.refusal.delta","item_id":"msg_a","output_index":0,"content_index":0,"delta":"I can't"}`,
		`{"type":"response.completed","response":{"id":"resp_a","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`,
		`{"type":"response.created","response":{"id":"resp_b","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_b","delta":"partial"}`,
		`{"type":"response.incomplete","response":{"id":"resp_b","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"output_tokens":4}}}`,
		`{"type":"response.created","response":{"id":"resp_c","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_c","delta":"cut"}`,
	}
	upstreamFrames := make([]passthroughTestFrame, 0, len(frames))
	for _, f := range frames {
		upstreamFrames = append(upstreamFrames, passthroughTestFrame{msgType: coderws.MessageText, payload: []byte(f)})
	}
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn(upstreamFrames, true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	turns := make([]RelayTurnResult, 0, 2)
	result, _ := Relay(ctx, clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-x","input":[]}`), RelayOptions{
		OnTurnComplete: func(turn RelayTurnResult) { turns = append(turns, turn) },
	})

	require.Len(t, turns, 2, "resp_c never reached a terminal event and must not emit a turn")
	require.Equal(t, "resp_a", turns[0].RequestID)
	require.NotNil(t, turns[0].ResponseOutcome)
	require.Equal(t, responseoutcome.ReasonRefusal, turns[0].ResponseOutcome.FinishReason())

	require.Equal(t, "resp_b", turns[1].RequestID)
	require.NotNil(t, turns[1].ResponseOutcome)
	require.False(t, turns[1].ResponseOutcome.Refusal, "resp_a refusal must not leak into resp_b")
	require.Equal(t, "max_output_tokens", turns[1].ResponseOutcome.IncompleteReason)
	require.Equal(t, responseoutcome.ReasonTruncated, turns[1].ResponseOutcome.FinishReason())

	require.NotNil(t, result.ResponseOutcome)
	require.Equal(t, responseoutcome.ReasonTruncated, result.ResponseOutcome.FinishReason(), "relay result reports the last completed response")
}
