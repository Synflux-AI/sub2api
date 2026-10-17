//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/responseoutcome"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newResponseOutcomeTestContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	return c, rec
}

func sseUpstream(sse string) *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
}

func TestResponseOutcome_AnthropicDirectStreamingRefusal(t *testing.T) {
	svc := newStreamingResponseTestGatewayService()
	c, _ := newResponseOutcomeTestContext("/v1/messages", nil)
	beginUpstreamResponseModelObservation(c)

	sse := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"stop_reason\":null,\"usage\":{\"input_tokens\":3}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"type\":\"refusal\",\"category\":\"cyber\"}},\"usage\":{\"output_tokens\":0}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sse))}

	_, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "claude-x", "claude-x", false)
	require.NoError(t, err)

	outcome := ResponseOutcomeFromContext(c)
	require.NotNil(t, outcome)
	require.Equal(t, responseoutcome.ProtocolAnthropicMessages, outcome.Protocol)
	require.Equal(t, "refusal", outcome.StopReason)
	require.Equal(t, "cyber", outcome.StopDetailsCategory)
	require.True(t, outcome.Terminal)
	require.Equal(t, responseoutcome.ReasonRefusal, outcome.FinishReason())
}

// 兼容转换会把 Anthropic max_tokens 映射成 Chat 的 length；观测必须在转换前保留上游原始语义。
func TestResponseOutcome_GatewayChatCompatCapturesUpstreamSemantics(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"model_context_window_exceeded\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, stream := range []bool{true, false} {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`)
		c, _ := newResponseOutcomeTestContext("/v1/chat/completions", body)
		svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: sseUpstream(sse)}
		account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test"}}

		_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
		require.NoError(t, err)

		outcome := ResponseOutcomeFromContext(c)
		require.NotNil(t, outcome, "stream=%v", stream)
		require.Equal(t, responseoutcome.ProtocolAnthropicMessages, outcome.Protocol)
		require.Equal(t, "model_context_window_exceeded", outcome.StopReason)
		require.True(t, outcome.HasOutput)
		require.Equal(t, responseoutcome.ReasonTruncated, outcome.FinishReason())
	}
}

func TestResponseOutcome_NativeAnthropicChatPauseTurn(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"k3\",\"content\":[]}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srv_1\",\"name\":\"web_search\",\"input\":{}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"pause_turn\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := []byte(`{"model":"k3","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c, _ := newResponseOutcomeTestContext("/v1/chat/completions", body)
	beginUpstreamResponseModelObservation(c)
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: sseUpstream(sse)}

	_, err := svc.forwardChatCompletionsViaNativeAnthropic(context.Background(), c, nativeAnthropicTestAccount(), body, "")
	require.NoError(t, err)

	outcome := ResponseOutcomeFromContext(c)
	require.NotNil(t, outcome)
	require.False(t, outcome.ToolCall, "server_tool_use runs on the server, not the client")
	require.True(t, outcome.HasOutput)
	require.Equal(t, responseoutcome.ReasonPauseTurn, outcome.FinishReason(), "pause_turn is normal control flow, not refusal or failure")
}

func TestResponseOutcome_OpenAIChatOverResponsesRefusalEvents(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-x\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"I can't\"}\n\n" +
		"event: response.refusal.done\ndata: {\"type\":\"response.refusal.done\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"refusal\":\"I can't help with that.\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-x\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"I can't help with that.\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"
	body := []byte(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c, _ := newResponseOutcomeTestContext("/v1/chat/completions", body)
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: sseUpstream(sse)}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")
	require.NoError(t, err)

	outcome := ResponseOutcomeFromContext(c)
	require.NotNil(t, outcome)
	require.Equal(t, responseoutcome.ProtocolOpenAIResponses, outcome.Protocol)
	require.Equal(t, "completed", outcome.Status)
	require.True(t, outcome.Refusal)
	require.Equal(t, responseoutcome.ReasonRefusal, outcome.FinishReason(), "completed status must not hide a refusal")
}

func TestResponseOutcome_RawChatDeltaRefusalSurvivesUsageChunk(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"but I can't continue\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c, _ := newResponseOutcomeTestContext("/v1/chat/completions", body)
	beginUpstreamResponseModelObservation(c)
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: sseUpstream(sse)}

	_, err := svc.forwardAsRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
	require.NoError(t, err)

	outcome := ResponseOutcomeFromContext(c)
	require.NotNil(t, outcome)
	require.Equal(t, responseoutcome.ProtocolOpenAIChatCompletions, outcome.Protocol)
	require.Equal(t, "stop", outcome.StopReason, "final usage chunk with empty choices must not overwrite")
	require.Equal(t, responseoutcome.ReasonRefusal, outcome.FinishReason())
}

// 每次 Forward 尝试都新建观测器：换号后只保留最终账号的结束原因。
func TestResponseOutcome_ResetPerForwardAttempt(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test"}}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c, _ := newResponseOutcomeTestContext("/v1/chat/completions", body)

	first := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[]}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: sseUpstream(first)}
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
	require.NoError(t, err)
	require.Equal(t, responseoutcome.ReasonRefusal, ResponseOutcomeFromContext(c).FinishReason())

	second := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[]}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	c2, _ := newResponseOutcomeTestContext("/v1/chat/completions", body)
	// 复用同一个请求上下文模拟 handler 层换号重试。
	c2.Keys = c.Keys
	svc.httpUpstream = sseUpstream(second)
	_, err = svc.ForwardAsChatCompletions(context.Background(), c2, account, body, nil)
	require.NoError(t, err)
	outcome := ResponseOutcomeFromContext(c2)
	require.False(t, outcome.Refusal, "previous attempt's refusal must not leak into the final attempt")
	require.Equal(t, responseoutcome.ReasonStop, outcome.FinishReason())
}

func TestResponseOutcome_NotCollectedStaysNil(t *testing.T) {
	c, _ := newResponseOutcomeTestContext("/v1/messages", nil)
	require.Nil(t, ResponseOutcomeFromContext(c))
	beginUpstreamResponseModelObservation(c)
	require.Nil(t, ResponseOutcomeFromContext(c), "an attempt that never saw upstream data must stay NULL")
	require.Nil(t, usageFinishReasonPtr(nil))
}

func TestResponseOutcome_RecordUsagePersistsFinishReason(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	outcome := &responseoutcome.Outcome{
		Protocol:         responseoutcome.ProtocolOpenAIResponses,
		Status:           "incomplete",
		IncompleteReason: "content_filter",
		HasOutput:        true,
		Terminal:         true,
	}

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID:       "resp_outcome",
			Usage:           OpenAIUsage{InputTokens: 10, OutputTokens: 2},
			Model:           "gpt-5.1",
			Duration:        time.Second,
			ResponseOutcome: outcome,
		},
		APIKey:  &APIKey{ID: 1, User: &User{ID: 2}},
		User:    &User{ID: 2},
		Account: &Account{ID: 3, Type: AccountTypeAPIKey},
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, usageRepo.lastLog.FinishReason)
	require.Equal(t, responseoutcome.ReasonContentFilter, *usageRepo.lastLog.FinishReason)
	require.Same(t, outcome, usageRepo.lastLog.ResponseOutcome)
	require.Greater(t, usageRepo.lastLog.ActualCost, 0.0, "observation must not change billing")

	usageRepo.lastLog = nil
	err = svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result:  &OpenAIForwardResult{RequestID: "resp_no_outcome", Usage: OpenAIUsage{InputTokens: 1}, Model: "gpt-5.1"},
		APIKey:  &APIKey{ID: 1, User: &User{ID: 2}},
		User:    &User{ID: 2},
		Account: &Account{ID: 3, Type: AccountTypeAPIKey},
	})
	require.NoError(t, err)
	require.Nil(t, usageRepo.lastLog.FinishReason, "uncollected paths keep NULL")
	require.Nil(t, usageRepo.lastLog.ResponseOutcome)
}

func TestResponseOutcome_RecordUsageSkipsOutcomeForNormalEndings(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	record := func(outcome *responseoutcome.Outcome) *UsageLog {
		usageRepo.lastLog = nil
		err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
			Result:  &OpenAIForwardResult{RequestID: "resp_" + outcome.Protocol + outcome.Status + outcome.StopReason, Usage: OpenAIUsage{InputTokens: 1}, Model: "gpt-5.1", ResponseOutcome: outcome},
			APIKey:  &APIKey{ID: 1, User: &User{ID: 2}},
			User:    &User{ID: 2},
			Account: &Account{ID: 3, Type: AccountTypeAPIKey},
		})
		require.NoError(t, err)
		require.NotNil(t, usageRepo.lastLog)
		return usageRepo.lastLog
	}

	stopped := record(&responseoutcome.Outcome{Protocol: responseoutcome.ProtocolAnthropicMessages, StopReason: "end_turn", HasOutput: true, Terminal: true})
	require.Equal(t, responseoutcome.ReasonStop, *stopped.FinishReason)
	require.Nil(t, stopped.ResponseOutcome, "normal endings keep only finish_reason")

	tool := record(&responseoutcome.Outcome{Protocol: responseoutcome.ProtocolOpenAIChatCompletions, StopReason: "tool_calls", ToolCall: true, HasOutput: true, Terminal: true})
	require.Equal(t, responseoutcome.ReasonToolUse, *tool.FinishReason)
	require.Nil(t, tool.ResponseOutcome)

	disconnected := record(&responseoutcome.Outcome{Protocol: responseoutcome.ProtocolOpenAIResponses, Status: "completed", HasOutput: true, Terminal: true, ClientDisconnected: true})
	require.Equal(t, responseoutcome.ReasonStop, *disconnected.FinishReason, "upstream finished; disconnect does not change the reason")
	require.NotNil(t, disconnected.ResponseOutcome, "client disconnect is a transport fact worth keeping")
	require.True(t, disconnected.ResponseOutcome.ClientDisconnected)

	paused := record(&responseoutcome.Outcome{Protocol: responseoutcome.ProtocolAnthropicMessages, StopReason: "pause_turn", HasOutput: true, Terminal: true})
	require.NotNil(t, paused.ResponseOutcome)
}

func TestResponseOutcome_BedrockNonStreamingRefusal(t *testing.T) {
	svc := newStreamingResponseTestGatewayService()
	c, _ := newResponseOutcomeTestContext("/v1/messages", nil)
	beginUpstreamResponseModelObservation(c)
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},"usage":{"input_tokens":3,"output_tokens":0}}`
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}

	_, err := svc.handleBedrockNonStreamingResponse(context.Background(), resp, c, &Account{ID: 1})
	require.NoError(t, err)

	outcome := ResponseOutcomeFromContext(c)
	require.NotNil(t, outcome, "Bedrock speaks Anthropic Messages and must be collected")
	require.Equal(t, responseoutcome.ProtocolAnthropicMessages, outcome.Protocol)
	require.Equal(t, "cyber", outcome.StopDetailsCategory)
	require.Equal(t, responseoutcome.ReasonRefusal, outcome.FinishReason())
}

func TestResponseOutcome_RawChatDoneWithoutFinishReasonIsUnknown(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c, _ := newResponseOutcomeTestContext("/v1/chat/completions", body)
	beginUpstreamResponseModelObservation(c)
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: sseUpstream(sse)}

	_, err := svc.forwardAsRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
	require.NoError(t, err)

	outcome := ResponseOutcomeFromContext(c)
	require.NotNil(t, outcome)
	require.True(t, outcome.Terminal, "[DONE] ends the stream")
	require.True(t, outcome.MissingFinishReason)
	require.Equal(t, responseoutcome.ReasonUnknown, outcome.FinishReason(), "missing finish_reason is unknown, not interrupted")
}
