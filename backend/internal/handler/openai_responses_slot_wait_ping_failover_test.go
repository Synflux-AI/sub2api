//go:build unit

package handler

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	slotWaitPingCreated  = `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}`
	slotWaitPingOverload = `{"type":"error","sequence_number":1,"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`
	slotWaitPingFailed   = `{"type":"response.failed","sequence_number":2,"response":{"id":"resp_1","status":"failed","error":{"code":"server_error","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}}`
	slotWaitPingDelta    = `{"type":"response.output_text.delta","sequence_number":1,"output_index":0,"content_index":0,"delta":"second-account-text"}`
	slotWaitPingDone     = `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"second-account-text"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`
)

func slotWaitPingSSE(events ...[2]string) *http.Response {
	var b strings.Builder
	for _, event := range events {
		b.WriteString("event: " + event[0] + "\ndata: " + event[1] + "\n\n")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(b.String())),
	}
}

// #298：客户端在排队等槽期间已收到 SSE ping（200 + SSE 头已提交），第一次尝试
// 开流后在可见输出前过载。期望重新调度上游，客户端以合法 SSE 续写收到第二次
// 尝试的完整流，且看不到第一次尝试的过载。
func TestOpenAIGatewayHandlerResponses_OverloadAfterSlotWaitPingFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, extra := range map[string]map[string]any{
		"native":      nil,
		"passthrough": {"openai_passthrough": true},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := newAstraProCapturedUpstream(
				slotWaitPingSSE(
					[2]string{"response.created", slotWaitPingCreated},
					[2]string{"error", slotWaitPingOverload},
					[2]string{"response.failed", slotWaitPingFailed},
				),
				slotWaitPingSSE(
					[2]string{"response.created", slotWaitPingCreated},
					[2]string{"response.output_text.delta", slotWaitPingDelta},
					[2]string{"response.completed", slotWaitPingDone},
				),
			)
			handler := newOpenAIResponsesFailoverTestHandlerWithExtra(t, upstream, extra)
			c, rec := newAstraProFailoverContext(t, `{"model":"gpt-5.1","stream":true,"input":"hello"}`)

			// 模拟 waitForSlotWithPing 已写出的两个 ping。
			c.Header("Content-Type", "text/event-stream")
			for range 2 {
				n, err := c.Writer.Write([]byte(":\n\n"))
				require.NoError(t, err)
				recordGatewayStreamHeartbeat(c, n)
			}

			handler.Responses(c)

			_, accountIDs, _ := upstream.snapshot()
			// 过载属于 request-scoped transient，现有策略先同号重试；这里只关心
			// 第一次尝试的流内过载没有直达客户端，而是触发了下一次上游尝试。
			require.Len(t, accountIDs, 2, "overload before visible output must trigger another upstream attempt")
			body := rec.Body.String()
			require.Equal(t, http.StatusOK, rec.Code)
			require.True(t, strings.HasPrefix(body, ":\n\n:\n\n"), "slot-wait pings stay at the head of the stream")
			require.Contains(t, body, "second-account-text")
			require.Contains(t, body, "response.completed")
			require.NotContains(t, body, "overloaded")
			require.NotContains(t, body, "response.failed")
			require.Equal(t, 1, strings.Count(body, "event: response.created"), "only the winning account's lifecycle reaches the client")
		})
	}
}
