//go:build unit

package service

// 国产供应商（kimi/zhipu/deepseek/minimax）原生 Anthropic 直通路径的 X-Trace-Id 注入。
//
// 回归背景（issue #241）：buildNativeAnthropicUpstreamRequest 是三条入站协议
// （/v1/messages、/v1/responses、/v1/chat/completions）落到供应商原生 Anthropic
// 端点的唯一出站构造点，此前全程未调用 injectTraceHeader；而 x-trace-id 按设计
// 也不在 allowedHeaders 白名单内（见 trace_header.go 注释），两条来源同时落空，
// 导致上游同为 sub2api 实例时两跳日志无法按 trace_id 关联。

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cnNativeAnthropicTraceAccount 构造走原生 Anthropic 直通路径的国产供应商账号。
func cnNativeAnthropicTraceAccount(protocol string, tracePassthrough bool) *Account {
	account := &Account{
		ID:          741,
		Name:        "kimi-native-trace",
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"api_protocol": protocol,
			// anthropic 协议取 base_url，adaptive 协议取 api_base_urls[anthropic]，
			// 两者都填成同一地址，避免回落到供应商默认 base 打到真实域名。
			"base_url": "http://anthropic.example",
			"api_base_urls": map[string]any{
				APIProtocolAnthropic: "http://anthropic.example",
			},
		},
	}
	if tracePassthrough {
		account.Extra = map[string]any{"trace_id_passthrough": true}
	}
	return account
}

// cnNativeAnthropicTraceIngressCases 覆盖 buildNativeAnthropicUpstreamRequest 的
// 三个调用点各自的入站协议。
func cnNativeAnthropicTraceIngressCases() []cnProtocolIngressCase {
	return []cnProtocolIngressCase{
		{
			name: "messages",
			path: "/v1/messages",
			body: []byte(`{"model":"k3","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hello"}]}`),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.ForwardAsAnthropic(traceIDContext(), c, account, body, "", "")
				return err
			},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"k3","input":"hello","stream":false}`),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.Forward(traceIDContext(), c, account, body)
				return err
			},
		},
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"k3","messages":[{"role":"user","content":"hello"}],"stream":false}`),
			forward: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.ForwardAsChatCompletions(traceIDContext(), c, account, body, "", "")
				return err
			},
		},
	}
}

const cnNativeAnthropicTraceValue = "trace-cn-native-241"

func traceIDContext() context.Context {
	// 前后带空白：顺带锁定出站值是 traceid.Normalize 后的结果，而非 ctx 原始值。
	return context.WithValue(context.Background(), ctxkey.TraceID, "  "+cnNativeAnthropicTraceValue+"  ")
}

func TestCNNativeAnthropicInjectsTraceHeaderWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range cnNativeAnthropicTraceIngressCases() {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c := adaptiveProtocolTestContext(tc.path, tc.body)
			// 客户端伪造的同名头：x-trace-id 不在 allowedHeaders，不得被拷贝到出站，
			// 更不得与服务端注入值在 wire 上同时出现。
			c.Request.Header.Set(traceid.Header, "client-forged-id")

			err := tc.forward(svc, c, cnNativeAnthropicTraceAccount(APIProtocolAnthropic, true), tc.body)
			require.Error(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, "http://anthropic.example/v1/messages", upstream.lastReq.URL.String())

			values := upstream.lastReq.Header.Values(traceid.Header)
			require.Equal(t, []string{cnNativeAnthropicTraceValue}, values)
		})
	}
}

func TestCNNativeAnthropicSkipsTraceHeaderWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range cnNativeAnthropicTraceIngressCases() {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			c := adaptiveProtocolTestContext(tc.path, tc.body)
			c.Request.Header.Set(traceid.Header, "client-forged-id")

			err := tc.forward(svc, c, cnNativeAnthropicTraceAccount(APIProtocolAnthropic, false), tc.body)
			require.Error(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Empty(t, upstream.lastReq.Header.Values(traceid.Header))
		})
	}
}

// adaptive 是这族账号的常见配置：/v1/messages 入站同样分流到原生 Anthropic 直通。
func TestCNNativeAnthropicAdaptiveProtocolInjectsTraceHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"k3","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hello"}]}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(traceIDContext(), adaptiveProtocolTestContext("/v1/messages", body),
		cnNativeAnthropicTraceAccount(APIProtocolAdaptive, true), body, "", "")
	require.Error(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, []string{cnNativeAnthropicTraceValue}, upstream.lastReq.Header.Values(traceid.Header))
}

// 流式请求走 detachStreamUpstreamContext(context.WithoutCancel)：只切断取消传播、
// value 仍可读，注入点取的仍是入站链路的 trace id。
func TestCNNativeAnthropicStreamingInjectsTraceHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"k3","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(traceIDContext(), adaptiveProtocolTestContext("/v1/messages", body),
		cnNativeAnthropicTraceAccount(APIProtocolAnthropic, true), body, "", "")
	require.Error(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, []string{cnNativeAnthropicTraceValue}, upstream.lastReq.Header.Values(traceid.Header))
}
