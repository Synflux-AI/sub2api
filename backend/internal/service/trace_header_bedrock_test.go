//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
	"github.com/stretchr/testify/require"
)

func bedrockTraceAccount(enabled bool) *Account {
	acc := &Account{
		ID:   31,
		Name: "acc-bedrock-trace",
		Type: AccountTypeAPIKey,
	}
	if enabled {
		acc.Extra = map[string]any{"trace_id_passthrough": true}
	}
	return acc
}

// TestBuildUpstreamRequestBedrock_TraceHeaderIsSigned 验证 SigV4 路径：
// X-Trace-Id 必须在 SignRequest 之前注入，因此签名后它应出现在 Authorization 的
// SignedHeaders 列表里——否则 wire 上的头与签名不一致，AWS 会拒绝请求。
func TestBuildUpstreamRequestBedrock_TraceHeaderIsSigned(t *testing.T) {
	const traceValue = "trace-bedrock-1"
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, traceValue)
	signer := NewBedrockSigner("AKIAEXAMPLE", "secretexample", "", "us-east-1")
	svc := &GatewayService{}

	body := []byte(`{"messages":[]}`)
	req, err := svc.buildUpstreamRequestBedrock(ctx, body, "anthropic.claude-3-5-sonnet-20241022-v2:0", "us-east-1", false, signer, bedrockTraceAccount(true))
	require.NoError(t, err)
	require.Equal(t, traceValue, req.Header.Get(traceid.Header))

	auth := req.Header.Get("Authorization")
	require.NotEmpty(t, auth, "应已完成 SigV4 签名")
	signedHeaders := ""
	for _, part := range strings.Split(auth, " ") {
		if strings.HasPrefix(part, "SignedHeaders=") {
			signedHeaders = strings.TrimSuffix(strings.TrimPrefix(part, "SignedHeaders="), ",")
		}
	}
	require.NotEmpty(t, signedHeaders, "Authorization 应包含 SignedHeaders")
	require.Contains(t, strings.Split(signedHeaders, ";"), strings.ToLower(traceid.Header),
		"x-trace-id 应被纳入签名，证明注入发生在 SignRequest 之前")
}

// TestBuildUpstreamRequestBedrock_ToggleOffKeepsSignedHeaders 对照组：
// 开关关闭时不写头，SignedHeaders 与今天保持一致。
func TestBuildUpstreamRequestBedrock_ToggleOffKeepsSignedHeaders(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-should-not-leak")
	signer := NewBedrockSigner("AKIAEXAMPLE", "secretexample", "", "us-east-1")
	svc := &GatewayService{}

	req, err := svc.buildUpstreamRequestBedrock(ctx, []byte(`{"messages":[]}`), "anthropic.claude-3-5-sonnet-20241022-v2:0", "us-east-1", false, signer, bedrockTraceAccount(false))
	require.NoError(t, err)
	require.Empty(t, req.Header.Get(traceid.Header))
	require.NotContains(t, strings.ToLower(req.Header.Get("Authorization")), strings.ToLower(traceid.Header))
}

// TestApplyCustomHeaders_CannotOverwriteTraceHeader 锁住 x-trace-id 进入
// protectedCustomHeaderNames 这一决定：自定义 header 不得覆盖已注入的链路 ID。
// 在 Bedrock SigV4 路径上这不只是语义问题——ApplyCustomHeaders 在 SignRequest 之后执行，
// 覆写一个已签名的 header 会让 wire 值与 SignedHeaders 不一致，AWS 返回 403。
func TestApplyCustomHeaders_CannotOverwriteTraceHeader(t *testing.T) {
	const traceValue = "trace-protected-1"
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, traceValue)
	signer := NewBedrockSigner("AKIAEXAMPLE", "secretexample", "", "us-east-1")
	svc := &GatewayService{}

	account := bedrockTraceAccount(true)
	account.CustomHeadersEnabled = true
	account.CustomHeaders = map[string]string{
		"X-Trace-Id":   "custom-override-attempt",
		"x-trace-id":   "custom-override-lowercase",
		"X-Extra-Head": "passes-through",
	}

	req, err := svc.buildUpstreamRequestBedrock(ctx, []byte(`{"messages":[]}`), "anthropic.claude-3-5-sonnet-20241022-v2:0", "us-east-1", false, signer, account)
	require.NoError(t, err)

	// 模拟真实调用序列：executeBedrockUpstream 在签名之后调用 ApplyCustomHeaders
	account.ApplyCustomHeaders(req)

	require.Equal(t, traceValue, req.Header.Get(traceid.Header),
		"自定义 header 不得覆盖已注入（且已签名）的 X-Trace-Id")
	require.Len(t, req.Header.Values(traceid.Header), 1)
	require.Equal(t, "passes-through", req.Header.Get("X-Extra-Head"),
		"非受保护的自定义 header 仍应生效")
}

// TestBuildUpstreamRequestBedrockAPIKey_TraceHeader 覆盖 Bearer Token 路径（无签名约束）。
func TestBuildUpstreamRequestBedrockAPIKey_TraceHeader(t *testing.T) {
	const traceValue = "trace-bedrock-apikey"
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, traceValue)
	svc := &GatewayService{}

	req, err := svc.buildUpstreamRequestBedrockAPIKey(ctx, []byte(`{"messages":[]}`), "anthropic.claude-3-5-sonnet-20241022-v2:0", "us-east-1", true, "bedrock-api-key", bedrockTraceAccount(true))
	require.NoError(t, err)
	require.Equal(t, traceValue, req.Header.Get(traceid.Header))
	require.Len(t, req.Header.Values(traceid.Header), 1)

	off, err := svc.buildUpstreamRequestBedrockAPIKey(ctx, []byte(`{"messages":[]}`), "anthropic.claude-3-5-sonnet-20241022-v2:0", "us-east-1", true, "bedrock-api-key", bedrockTraceAccount(false))
	require.NoError(t, err)
	require.Empty(t, off.Header.Get(traceid.Header))
}
