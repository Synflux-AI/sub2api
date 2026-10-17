package responseoutcome

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func feedAnthropic(o *Observer, events ...string) {
	for _, e := range events {
		o.ObserveAnthropicEvent([]byte(e))
	}
}

func feedChat(o *Observer, chunks ...string) {
	for _, c := range chunks {
		o.ObserveChatChunk([]byte(c))
	}
}

func feedResponses(o *Observer, events ...string) {
	for _, e := range events {
		o.ObserveResponsesEvent([]byte(e))
	}
}

func TestNilObserverIsNoop(t *testing.T) {
	var o *Observer
	o.ObserveAnthropicEvent([]byte(`{"type":"message_stop"}`))
	o.ObserveChatCompletion([]byte(`{}`))
	require.Nil(t, o.Snapshot())
	var out *Outcome
	require.Empty(t, out.FinishReason())
}

func TestSnapshotNilWhenNothingObserved(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	o.ObserveResponsesEvent([]byte(`not json`))
	o.ObserveResponse([]byte(`{"foo":1}`))
	require.Nil(t, o.Snapshot(), "unobserved paths must stay NULL rather than default to stop")
}

func TestAnthropicStreamingStopReasons(t *testing.T) {
	cases := []struct {
		stopReason string
		want       string
	}{
		{"end_turn", ReasonStop},
		{"stop_sequence", ReasonStop},
		{"tool_use", ReasonToolUse},
		{"pause_turn", ReasonPauseTurn},
		{"max_tokens", ReasonTruncated},
		{"model_context_window_exceeded", ReasonTruncated},
		{"refusal", ReasonRefusal},
		{"some_future_reason", ReasonUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.stopReason, func(t *testing.T) {
			o := NewObserver(ProtocolAnthropicMessages)
			feedAnthropic(o,
				`{"type":"message_start","message":{"stop_reason":null,"content":[]}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"`+tc.stopReason+`"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			)
			out := o.Snapshot()
			require.NotNil(t, out)
			require.Equal(t, tc.stopReason, out.StopReason, "raw reason must be preserved")
			require.Equal(t, tc.want, out.FinishReason())
		})
	}
}

func TestAnthropicMessageStartNullDoesNotFix(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	feedAnthropic(o, `{"type":"message_start","message":{"stop_reason":null}}`)
	out := o.Snapshot()
	require.Empty(t, out.StopReason)
	require.False(t, out.Terminal)
	require.Equal(t, ReasonInterrupted, out.FinishReason())
}

func TestAnthropicDeltaWithoutMessageStopIsInterrupted(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	feedAnthropic(o,
		`{"type":"message_start","message":{}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	)
	out := o.Snapshot()
	require.Equal(t, "end_turn", out.StopReason)
	require.Equal(t, ReasonInterrupted, out.FinishReason())
}

func TestAnthropicRefusalStopDetails(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	o.ObserveAnthropicMessage([]byte(`{"type":"message","content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"}}`))
	out := o.Snapshot()
	require.True(t, out.Refusal)
	require.Equal(t, "refusal", out.StopDetailsType)
	require.Equal(t, "cyber", out.StopDetailsCategory)
	require.False(t, out.HasOutput)
	require.Equal(t, ReasonRefusal, out.FinishReason(), "refusal must not be reported as empty")
}

func TestAnthropicToolUseWithoutTextIsNotEmpty(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	o.ObserveAnthropicMessage([]byte(`{"type":"message","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}],"stop_reason":"tool_use"}`))
	out := o.Snapshot()
	require.True(t, out.ToolCall)
	require.True(t, out.HasOutput)
	require.Equal(t, ReasonToolUse, out.FinishReason())
}

func TestAnthropicThinkingOnlyIsEmpty(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	o.ObserveAnthropicMessage([]byte(`{"type":"message","content":[{"type":"thinking","thinking":"..."}],"stop_reason":"end_turn"}`))
	require.Equal(t, ReasonEmpty, o.Snapshot().FinishReason())
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	feedAnthropic(o,
		`{"type":"message_start","message":{}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"x"}}`,
	)
	out := o.Snapshot()
	require.Equal(t, "overloaded_error", out.ErrorType)
	require.Equal(t, ReasonFailed, out.FinishReason())
}

func TestAnthropicDuplicateStopReasonFirstWins(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	feedAnthropic(o,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`{"type":"message_stop"}`,
	)
	require.Equal(t, "max_tokens", o.Snapshot().StopReason)
}

func TestChatStopWithRefusal(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	o.ObserveChatCompletion([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}]}`))
	out := o.Snapshot()
	require.Equal(t, "stop", out.StopReason)
	require.True(t, out.Refusal)
	require.Equal(t, ReasonRefusal, out.FinishReason(), "finish_reason=stop alone must not hide a refusal")
}

func TestChatStreamingDeltaRefusal(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	feedChat(o,
		`{"choices":[{"index":0,"delta":{"role":"assistant","refusal":""}}]}`,
		`{"choices":[{"index":0,"delta":{"refusal":"Sorry"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	)
	out := o.Snapshot()
	require.True(t, out.Refusal)
	require.Equal(t, "stop", out.StopReason, "final usage chunk with empty choices must not overwrite")
	require.Equal(t, ReasonRefusal, out.FinishReason())
}

func TestChatToolCallsWithNullContent(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	o.ObserveChatCompletion([]byte(`{"choices":[{"index":0,"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	out := o.Snapshot()
	require.True(t, out.HasOutput)
	require.Equal(t, ReasonToolUse, out.FinishReason())
}

func TestChatFinishReasons(t *testing.T) {
	cases := map[string]string{
		"stop":           ReasonStop,
		"length":         ReasonTruncated,
		"tool_calls":     ReasonToolUse,
		"function_call":  ReasonToolUse,
		"content_filter": ReasonContentFilter,
		"weird":          ReasonUnknown,
	}
	for raw, want := range cases {
		o := NewObserver(ProtocolOpenAIChatCompletions)
		feedChat(o,
			`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"`+raw+`"}]}`,
		)
		require.Equal(t, want, o.Snapshot().FinishReason(), raw)
	}
}

func TestChatMultipleChoices(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	o.ObserveChatCompletion([]byte(`{"choices":[
		{"index":0,"message":{"content":"a"},"finish_reason":"stop"},
		{"index":1,"message":{"content":"b"},"finish_reason":"length"}
	]}`))
	out := o.Snapshot()
	require.Equal(t, []string{"stop", "length"}, out.ChoiceFinishReasons)
	require.Equal(t, "length", out.StopReason)
	require.Equal(t, ReasonTruncated, out.FinishReason())
}

func TestChatStreamWithoutFinishIsInterrupted(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	feedChat(o, `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`)
	require.Equal(t, ReasonInterrupted, o.Snapshot().FinishReason())
}

func TestChatEmptyStop(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	feedChat(o,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	require.Equal(t, ReasonEmpty, o.Snapshot().FinishReason())
}

func TestResponsesCompletedWithRefusalBlock(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	o.ObserveResponse([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"I'm sorry"}]}]}`))
	out := o.Snapshot()
	require.Equal(t, "completed", out.Status)
	require.True(t, out.Refusal)
	require.Equal(t, ReasonRefusal, out.FinishReason())
}

func TestResponsesStreamingRefusalEvents(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.refusal.delta","delta":"I'm"}`,
		`{"type":"response.refusal.done","refusal":"I'm sorry"}`,
		// 部分上游的 completed 不回带 output，拒答事实必须来自前面的事件。
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
	)
	require.Equal(t, ReasonRefusal, o.Snapshot().FinishReason())
}

func TestResponsesIncompleteReasons(t *testing.T) {
	cases := map[string]string{
		"max_output_tokens": ReasonTruncated,
		"content_filter":    ReasonContentFilter,
		"max_messages":      ReasonIncomplete,
		"steered":           ReasonIncomplete,
		"":                  ReasonIncomplete,
	}
	for reason, want := range cases {
		o := NewObserver(ProtocolOpenAIResponses)
		feedResponses(o,
			`{"type":"response.output_text.delta","delta":"x"}`,
			`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"`+reason+`"}}}`,
		)
		out := o.Snapshot()
		require.Equal(t, reason, out.IncompleteReason)
		require.Equal(t, want, out.FinishReason(), reason)
	}
}

func TestResponsesMixedTextAndTool(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	o.ObserveResponse([]byte(`{"status":"completed","output":[
		{"type":"reasoning","summary":[]},
		{"type":"message","content":[{"type":"output_text","text":"calling"}]},
		{"type":"function_call","name":"f","arguments":"{}"}
	]}`))
	out := o.Snapshot()
	require.True(t, out.ToolCall)
	require.Equal(t, ReasonToolUse, out.FinishReason())
}

func TestResponsesFailedAndCancelled(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o, `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error"}}}`)
	out := o.Snapshot()
	require.Equal(t, "server_error", out.ErrorType)
	require.Equal(t, ReasonFailed, out.FinishReason())

	o = NewObserver(ProtocolOpenAIResponses)
	feedResponses(o, `{"type":"response.cancelled","response":{"status":"cancelled"}}`)
	require.Equal(t, ReasonCancelled, o.Snapshot().FinishReason())
}

func TestResponsesReasoningOnlyIsEmpty(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"response.output_item.done","item":{"type":"reasoning"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning"}]}}`,
	)
	require.Equal(t, ReasonEmpty, o.Snapshot().FinishReason())
}

func TestResponsesDuplicateTerminalFirstWins(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"response.output_text.delta","delta":"x"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	)
	require.Equal(t, ReasonTruncated, o.Snapshot().FinishReason())
}

func TestResponsesNoTerminalIsInterrupted(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.output_text.delta","delta":"partial"}`,
	)
	require.Equal(t, ReasonInterrupted, o.Snapshot().FinishReason())
}

func TestRefusalBeatsTruncation(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"response.refusal.delta","delta":"no"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	)
	out := o.Snapshot()
	require.Equal(t, ReasonRefusal, out.FinishReason())
	require.Equal(t, "max_output_tokens", out.IncompleteReason, "truncation fact must be preserved")
}

func TestRawValueTruncated(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	feedAnthropic(o, `{"type":"message_delta","delta":{"stop_reason":"`+string(long)+`"}}`)
	require.Len(t, o.Snapshot().StopReason, rawValueMaxLen)
}

func TestAbnormalReasonsAreValid(t *testing.T) {
	for _, r := range AbnormalReasons {
		require.True(t, IsValidReason(r))
	}
	require.False(t, IsValidReason("bogus"))
}

func TestParseFilter(t *testing.T) {
	got, err := ParseFilter("")
	require.NoError(t, err)
	require.Nil(t, got)

	got, err = ParseFilter(" Refusal , truncated,refusal ")
	require.NoError(t, err)
	require.Equal(t, []string{ReasonRefusal, ReasonTruncated}, got)

	got, err = ParseFilter("abnormal,unknown")
	require.NoError(t, err)
	require.Equal(t, append(append([]string{}, AbnormalReasons...), ReasonUnknown), got)
	require.NotContains(t, AbnormalReasons, ReasonUnknown, "unknown is shown separately, not as a failure")
	require.NotContains(t, AbnormalReasons, ReasonToolUse)
	require.NotContains(t, AbnormalReasons, ReasonPauseTurn)

	_, err = ParseFilter("refusal,bogus")
	require.Error(t, err)
}

func TestObserveOpenAIPayloadDispatch(t *testing.T) {
	o := NewObserver("")
	o.ObserveOpenAIPayload([]byte(`{"object":"chat.completion","choices":[{"index":0,"message":{"content":"x"},"finish_reason":"length"}]}`), "")
	require.Equal(t, ProtocolOpenAIChatCompletions, o.Snapshot().Protocol)
	require.Equal(t, ReasonTruncated, o.Snapshot().FinishReason())

	o = NewObserver("")
	o.ObserveOpenAIPayload([]byte(`{"object":"response","status":"completed","output":[{"type":"function_call","name":"f"}]}`), "")
	require.Equal(t, ProtocolOpenAIResponses, o.Snapshot().Protocol)
	require.Equal(t, ReasonToolUse, o.Snapshot().FinishReason())

	o = NewObserver("")
	o.ObserveOpenAIPayload([]byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}`), "")
	require.Equal(t, ReasonStop, o.Snapshot().FinishReason())

	o = NewObserver("")
	o.ObserveAnthropicPayload([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}],"stop_reason":"stop_sequence"}`))
	require.Equal(t, ReasonStop, o.Snapshot().FinishReason())
}

// 网关以裸 error 之后到达的成功终态为准；观测必须与之一致，不能记成 failed。
func TestResponsesBareErrorThenSuccessfulTerminal(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"response.output_text.delta","delta":"x"}`,
		`{"type":"error","error":{"type":"server_error"}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	)
	out := o.Snapshot()
	require.Empty(t, out.ErrorType)
	require.Equal(t, ReasonStop, out.FinishReason())
}

func TestResponsesBareErrorWithoutTerminalIsFailed(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"response.output_text.delta","delta":"x"}`,
		`{"type":"error","error":{"code":"rate_limit_exceeded"}}`,
	)
	out := o.Snapshot()
	require.Equal(t, "rate_limit_exceeded", out.ErrorType)
	require.True(t, out.Terminal)
	require.Equal(t, ReasonFailed, out.FinishReason())
}

func TestResponsesBareErrorThenFailedKeepsFailedCode(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	feedResponses(o,
		`{"type":"error","error":{"type":"invalid_prompt"}}`,
		`{"type":"response.failed","response":{"status":"failed"}}`,
	)
	out := o.Snapshot()
	require.Equal(t, "invalid_prompt", out.ErrorType)
	require.Equal(t, ReasonFailed, out.FinishReason())
}

// PR #303 review §2：Chat 终态判定。
func TestChatStreamMultiChoicePartialFinishThenDisconnectIsInterrupted(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	feedChat(o,
		`{"choices":[{"index":0,"delta":{"content":"a"}},{"index":1,"delta":{"content":"b"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	out := o.Snapshot()
	require.False(t, out.Terminal, "choice 1 never finished")
	require.Equal(t, []string{"stop", ""}, out.ChoiceFinishReasons)
	require.Equal(t, ReasonInterrupted, out.FinishReason())
}

func TestChatStreamMultiChoiceAllFinishedIsTerminal(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	feedChat(o,
		`{"choices":[{"index":0,"delta":{"content":"a"}},{"index":1,"delta":{"content":"b"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[{"index":1,"delta":{},"finish_reason":"length"}]}`,
	)
	out := o.Snapshot()
	require.True(t, out.Terminal)
	require.False(t, out.MissingFinishReason)
	require.Equal(t, ReasonTruncated, out.FinishReason())
}

func TestChatCompletionNullFinishReasonIsUnknown(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	o.ObserveChatCompletion([]byte(`{"object":"chat.completion","choices":[{"index":0,"message":{"content":"hi"},"finish_reason":null}]}`))
	out := o.Snapshot()
	require.True(t, out.Terminal, "a complete body is terminal by itself")
	require.True(t, out.MissingFinishReason)
	require.Equal(t, ReasonUnknown, out.FinishReason())
}

func TestChatStreamDoneWithoutFinishReasonIsUnknown(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	o.ObserveOpenAIPayload([]byte(`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`), "")
	o.ObserveOpenAIPayload([]byte(" [DONE] "), "")
	out := o.Snapshot()
	require.True(t, out.Terminal)
	require.Equal(t, ReasonUnknown, out.FinishReason(), "unknown is not counted as abnormal")
	require.NotContains(t, AbnormalReasons, out.FinishReason())
}

func TestChatCompletionEmptyChoicesIsEmpty(t *testing.T) {
	o := NewObserver(ProtocolOpenAIChatCompletions)
	o.ObserveChatCompletion([]byte(`{"object":"chat.completion","choices":[]}`))
	out := o.Snapshot()
	require.NotNil(t, out, "a complete empty body is observed, not uncollected")
	require.True(t, out.Terminal)
	require.Equal(t, ReasonEmpty, out.FinishReason())
}

func TestDoneAloneDoesNotClaimProtocol(t *testing.T) {
	o := NewObserver("")
	o.ObserveOpenAIPayload([]byte("[DONE]"), "")
	require.Nil(t, o.Snapshot(), "[DONE] by itself must not mark anything observed")

	o.ObserveOpenAIPayload([]byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}]}}`), "response.completed")
	out := o.Snapshot()
	require.Equal(t, ProtocolOpenAIResponses, out.Protocol)
	require.Equal(t, ReasonStop, out.FinishReason())
}

// PR #303 review §7：服务端执行的工具不算客户端工具调用。
func TestResponsesMCPCallIsServerSide(t *testing.T) {
	o := NewObserver(ProtocolOpenAIResponses)
	o.ObserveResponse([]byte(`{"status":"completed","output":[
		{"type":"mcp_call","name":"lookup","output":"..."},
		{"type":"web_search_call","status":"completed"},
		{"type":"message","content":[{"type":"output_text","text":"final answer"}]}
	]}`))
	out := o.Snapshot()
	require.False(t, out.ToolCall)
	require.Equal(t, ReasonStop, out.FinishReason())

	o = NewObserver(ProtocolOpenAIResponses)
	o.ObserveResponse([]byte(`{"status":"completed","output":[{"type":"mcp_approval_request","name":"lookup"}]}`))
	require.Equal(t, ReasonToolUse, o.Snapshot().FinishReason(), "approval requests need the client")
}

func TestAnthropicServerToolUseIsNotClientToolCall(t *testing.T) {
	o := NewObserver(ProtocolAnthropicMessages)
	o.ObserveAnthropicMessage([]byte(`{"type":"message","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{}},{"type":"text","text":"done"}],"stop_reason":"end_turn"}`))
	out := o.Snapshot()
	require.False(t, out.ToolCall)
	require.True(t, out.HasOutput)
	require.Equal(t, ReasonStop, out.FinishReason())
}
