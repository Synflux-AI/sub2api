//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// #265：资源中心把多家口径不同的中转挂在同一个下游账号后面，下游只能靠账号开关
// 猜 input_tokens 是总量还是净输入。CN 原生直通在转发出去的 usage 里声明本实例
// 已还原的总量后，下游无论开关开关都必须得到与上游实例记账一致的结果。

func nativeUsageSemanticAccount(openAISemantic bool) *Account {
	account := nativeAnthropicTestAccount()
	if openAISemantic {
		account.Extra = map[string]any{upstreamUsageOpenAISemanticExtraKey: true}
	}
	return account
}

func nativeUsageSemanticSSE(startUsage, deltaUsage string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"k3","content":[],"stop_reason":null,"usage":` + startUsage + "}}\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + deltaUsage + "}\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func nativeUsageSemanticBody(usage string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"k3",` +
		`"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":` + usage + `}`
}

// forwardNativeUsageSemanticHop 让一跳 CN 原生直通处理上游返回，得到本跳记账与转发出去的内容。
func forwardNativeUsageSemanticHop(t *testing.T, account *Account, stream bool, upstreamBody string) (OpenAIUsage, string) {
	t.Helper()
	contentType := "application/json"
	reqBody := []byte(`{"model":"k3","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if stream {
		contentType = "text/event-stream"
		reqBody = []byte(`{"model":"k3","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(reqBody)))
	c.Request.Header.Set("Content-Type", "application/json")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, reqBody, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	return result.Usage, recorder.Body.String()
}

func TestNativeAnthropicDeclaresUsageSemanticForDownstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name           string
		upstreamFlag   bool // 上游实例（资源中心）该渠道账号是否开了开关
		stream         bool
		upstream       string
		wantInput      int // 上游实例应记的净输入
		wantCacheRead  int
		wantCacheWrite int
	}{
		{
			name:         "含缓存渠道_K3开头全0",
			upstreamFlag: true, stream: true,
			upstream:  nativeUsageSemanticSSE(`{"input_tokens":0,"output_tokens":0}`, `{"input_tokens":108006,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 1510, wantCacheRead: 106496,
		},
		{
			name:         "净输入渠道",
			upstreamFlag: false, stream: true,
			upstream:  nativeUsageSemanticSSE(`{"input_tokens":0,"output_tokens":0}`, `{"input_tokens":1510,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 1510, wantCacheRead: 106496,
		},
		{
			name:         "官方形态自带prompt_tokens",
			upstreamFlag: false, stream: true,
			upstream: nativeUsageSemanticSSE(
				`{"input_tokens":108006,"prompt_tokens":108006,"cache_read_input_tokens":106496,"output_tokens":1}`,
				`{"input_tokens":1510,"prompt_tokens":108006,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 1510, wantCacheRead: 106496,
		},
		{
			name:         "上游自带billing_usage声明",
			upstreamFlag: false, stream: true,
			upstream: nativeUsageSemanticSSE(
				`{"input_tokens":50722,"cache_read_input_tokens":50457,"output_tokens":1,`+
					`"billing_usage":{"source":"oai_chat","semantic":"openai","openai_usage":{"prompt_tokens":50722,"prompt_tokens_details":{"cached_tokens":50457}}}}`,
				`{"output_tokens":316}`),
			wantInput: 265, wantCacheRead: 50457,
		},
		{
			name:         "缓存全命中净输入为0",
			upstreamFlag: true, stream: true,
			upstream:  nativeUsageSemanticSSE(`{"input_tokens":0,"output_tokens":0}`, `{"input_tokens":106496,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 0, wantCacheRead: 106496,
		},
		{
			name:         "缓存只在delta出现",
			upstreamFlag: true, stream: true,
			upstream:  nativeUsageSemanticSSE(`{"input_tokens":108006,"output_tokens":1}`, `{"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 1510, wantCacheRead: 106496,
		},
		{
			name:         "delta重复带总量",
			upstreamFlag: true, stream: true,
			upstream: nativeUsageSemanticSSE(
				`{"input_tokens":108006,"cache_read_input_tokens":106496,"output_tokens":1}`,
				`{"input_tokens":108006,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 1510, wantCacheRead: 106496,
		},
		{
			name:         "含缓存写入",
			upstreamFlag: true, stream: true,
			upstream:  nativeUsageSemanticSSE(`{"input_tokens":0,"output_tokens":0}`, `{"input_tokens":5000,"cache_read_input_tokens":3000,"cache_creation_input_tokens":1200,"output_tokens":20}`),
			wantInput: 800, wantCacheRead: 3000, wantCacheWrite: 1200,
		},
		{
			name:         "无缓存",
			upstreamFlag: true, stream: true,
			upstream:  nativeUsageSemanticSSE(`{"input_tokens":93,"output_tokens":1}`, `{"output_tokens":16}`),
			wantInput: 93,
		},
		{
			name:         "非流式_含缓存渠道",
			upstreamFlag: true, stream: false,
			upstream:  nativeUsageSemanticBody(`{"input_tokens":108006,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 1510, wantCacheRead: 106496,
		},
		{
			name:         "非流式_净输入渠道",
			upstreamFlag: false, stream: false,
			upstream:  nativeUsageSemanticBody(`{"input_tokens":1510,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 1510, wantCacheRead: 106496,
		},
		{
			name:         "非流式_缓存全命中净输入为0",
			upstreamFlag: true, stream: false,
			upstream:  nativeUsageSemanticBody(`{"input_tokens":106496,"cache_read_input_tokens":106496,"output_tokens":200}`),
			wantInput: 0, wantCacheRead: 106496,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstreamUsage, forwarded := forwardNativeUsageSemanticHop(t, nativeUsageSemanticAccount(tc.upstreamFlag), tc.stream, tc.upstream)
			require.Equal(t, tc.wantInput, upstreamUsage.InputTokens-upstreamUsage.CacheReadInputTokens-upstreamUsage.CacheCreationInputTokens, "上游实例净输入")
			require.Equal(t, tc.wantCacheRead, upstreamUsage.CacheReadInputTokens)
			require.Equal(t, tc.wantCacheWrite, upstreamUsage.CacheCreationInputTokens)

			// 客户可见的外层 input_tokens 保持上游原值；非 usage 事件逐字节不变。
			if tc.stream {
				upstreamLines := strings.Split(tc.upstream, "\n")
				forwardedLines := strings.Split(forwarded, "\n")
				require.Len(t, forwardedLines, len(upstreamLines))
				for i, line := range upstreamLines {
					data, ok := extractAnthropicSSEDataLine(line)
					eventType := gjson.Get(data, "type").String()
					if !ok || (eventType != "message_start" && eventType != "message_delta") {
						require.Equal(t, line, forwardedLines[i])
						continue
					}
					got, _ := extractAnthropicSSEDataLine(forwardedLines[i])
					usagePath := "usage"
					if eventType == "message_start" {
						usagePath = "message.usage"
					}
					require.Equal(t, gjson.Get(data, usagePath+".input_tokens").Raw, gjson.Get(got, usagePath+".input_tokens").Raw)
				}
			} else {
				require.Equal(t, gjson.Get(tc.upstream, "usage.input_tokens").Raw, gjson.Get(forwarded, "usage.input_tokens").Raw)
			}

			for _, downstreamFlag := range []bool{true, false} {
				downstreamUsage, _ := forwardNativeUsageSemanticHop(t, nativeUsageSemanticAccount(downstreamFlag), tc.stream, forwarded)
				require.Equal(t, upstreamUsage, downstreamUsage, "下游开关=%v 必须与上游实例记账一致", downstreamFlag)
			}
		})
	}
}
