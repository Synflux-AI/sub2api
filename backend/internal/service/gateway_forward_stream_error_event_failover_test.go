//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// streamErrorEventUpstream 按调用次序返回预先准备好的响应体，并记录每次请求体，
// 用于断言流内签名整流的重发确实去掉了 thinking 块。
type streamErrorEventUpstream struct {
	mu       sync.Mutex
	bodies   []func() io.ReadCloser
	requests [][]byte
}

func (u *streamErrorEventUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var reqBody []byte
	if req != nil && req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
	}
	idx := len(u.requests)
	u.requests = append(u.requests, reqBody)
	if idx >= len(u.bodies) {
		idx = len(u.bodies) - 1
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       u.bodies[idx](),
	}, nil
}

func (u *streamErrorEventUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *streamErrorEventUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func staticStreamBody(s string) func() io.ReadCloser {
	return func() io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
}

// delayedStreamBody 先静默 delay 再写出 s，让本机 keepalive 心跳先于上游内容写给客户端。
func delayedStreamBody(delay time.Duration, s string) func() io.ReadCloser {
	return func() io.ReadCloser {
		reader, writer := io.Pipe()
		go func() {
			time.Sleep(delay)
			_, _ = io.WriteString(writer, s)
			_ = writer.Close()
		}()
		return reader
	}
}

const (
	streamErrorSignatureFrame  = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages.1.content.0: Invalid `signature` in `thinking` block\"}}\n\n"
	streamErrorPermissionFrame = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Internal server error\"}}\n\n"
	streamErrorSemanticPrefix  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	streamErrorSuccessStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":7}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func newStreamErrorEventForwardService(t *testing.T, upstream HTTPUpstream, keepaliveInterval int, rectifier *RectifierSettings) *GatewayService {
	t.Helper()
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	cfg := &config.Config{Gateway: config.GatewayConfig{
		MaxLineSize:             defaultMaxLineSize,
		StreamKeepaliveInterval: keepaliveInterval,
	}}
	svc := &GatewayService{
		cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream,
		settingService: NewSettingService(repo, cfg), rateLimitService: &RateLimitService{}, deferredService: &DeferredService{},
	}
	if rectifier != nil {
		require.NoError(t, svc.settingService.SetRectifierSettings(context.Background(), rectifier))
	}
	return svc
}

// newStreamErrorEventThinkingParsed 构造带上一轮 thinking 块（及其签名）的流式请求，
// 签名来自别的上游 —— 正是 #327 里当前上游拒收的形态。
func newStreamErrorEventThinkingParsed(t *testing.T) *ParsedRequest {
	t.Helper()
	body := []byte(`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":512},"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"pondering","signature":"sig-from-another-upstream"},{"type":"text","text":"hello"}]},` +
		`{"role":"user","content":[{"type":"text","text":"again"}]}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}

func apiKeySignatureRectifierOn() *RectifierSettings {
	return &RectifierSettings{Enabled: true, ThinkingSignatureEnabled: true, APIKeySignatureEnabled: true}
}

// #327 现象复现：上游静默超过一个心跳周期，本机已写出 event: ping，然后上游在
// 200 流里发 event:error。客户端只收到过非语义心跳，必须放行换号。
func TestForwardStreamErrorEventAfterLocalKeepaliveCanFailOver(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{
		delayedStreamBody(1100*time.Millisecond, streamErrorPermissionFrame),
	}}
	svc := newStreamErrorEventForwardService(t, upstream, 1, nil)
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusForbidden, failoverErr.StatusCode)
	// handler 的 gatewayForwardMayFailover 第一道闸门已被心跳字节推开，
	// 必须靠这个标记放行，否则直接 handleFailoverExhausted 不换号。
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.False(t, IsResponseCommitted(c))

	require.Contains(t, recorder.Body.String(), "event: ping")
	require.NotContains(t, recorder.Body.String(), "message_start")
	require.NotContains(t, recorder.Body.String(), "event: error")
	require.Equal(t, 1, upstream.calls())

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_error", events[0].Kind)
}

// 心跳之后才出现的 overloaded_error 也只算「未交付语义内容」，保持 529 语义。
func TestForwardStreamOverloadedErrorAfterLocalKeepaliveKeeps529(t *testing.T) {
	frame := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{delayedStreamBody(1100*time.Millisecond, frame)}}
	svc := newStreamErrorEventForwardService(t, upstream, 1, nil)
	svc.rateLimitService = &RateLimitService{accountRepo: &errorPolicyRepoStub{}, cfg: &config.Config{}}
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 529, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Contains(t, recorder.Body.String(), "event: ping")
}

// 已交付 content_block_delta 后再出现 error 帧：SSE 无法撤回，绝不能放行换号。
func TestForwardStreamErrorEventAfterSemanticOutputDoesNotFailOver(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{
		staticStreamBody(streamErrorSemanticPrefix + streamErrorPermissionFrame),
	}}
	svc := newStreamErrorEventForwardService(t, upstream, 0, nil)
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusForbidden, failoverErr.StatusCode)
	require.False(t, failoverErr.SafeToFailoverAfterWrite)
	require.Contains(t, recorder.Body.String(), "content_block_delta")
}

// 心跳 + 语义内容都写过：扣掉心跳后仍有语义输出，不得放行。
func TestForwardStreamErrorEventAfterKeepaliveAndSemanticOutputDoesNotFailOver(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{
		delayedStreamBody(1100*time.Millisecond, streamErrorSemanticPrefix+streamErrorPermissionFrame),
	}}
	svc := newStreamErrorEventForwardService(t, upstream, 1, nil)
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.False(t, failoverErr.SafeToFailoverAfterWrite)
	require.Contains(t, recorder.Body.String(), "event: ping")
	require.Contains(t, recorder.Body.String(), "content_block_delta")
}

// 流内签名错误：同号去掉 thinking 块重发一次，重发成功则正常交付。
func TestForwardStreamSignatureErrorRectifiesOnSameAccount(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{
		delayedStreamBody(1100*time.Millisecond, streamErrorSignatureFrame),
		staticStreamBody(streamErrorSuccessStream),
	}}
	svc := newStreamErrorEventForwardService(t, upstream, 1, apiKeySignatureRectifierOn())
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	parsed := newStreamErrorEventThinkingParsed(t)

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), parsed)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, 2, upstream.calls())
	require.Contains(t, string(upstream.requests[0]), `"type":"thinking"`)
	require.NotContains(t, string(upstream.requests[1]), `"type":"thinking"`, "重发必须去掉 thinking 块")
	require.NotContains(t, string(upstream.requests[1]), "sig-from-another-upstream")
	require.NotContains(t, string(parsed.Body.Bytes()), "sig-from-another-upstream", "ParsedRequest 要同步为实际交付的请求体")

	body := recorder.Body.String()
	require.Contains(t, body, "event: ping")
	require.Contains(t, body, "recovered")
	require.Equal(t, 1, strings.Count(body, "event: message_start"))
	require.NotContains(t, body, "event: error")

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_signature_error", events[0].Kind)
}

// 去 thinking 重发后流里仍是签名错误：不再重试，按 stream_error 换号，且心跳不挡换号。
func TestForwardStreamSignatureErrorPersistsAfterRectifyFailsOver(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{
		staticStreamBody(streamErrorSignatureFrame),
		delayedStreamBody(1100*time.Millisecond, streamErrorSignatureFrame),
	}}
	svc := newStreamErrorEventForwardService(t, upstream, 1, apiKeySignatureRectifierOn())
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newStreamErrorEventThinkingParsed(t))

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusForbidden, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Equal(t, 2, upstream.calls(), "只整流一次")
	require.Contains(t, recorder.Body.String(), "event: ping")

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 2)
	require.Equal(t, "stream_signature_error", events[0].Kind)
	require.Equal(t, "stream_error", events[1].Kind)
}

// API Key 签名整流开关关闭（默认）时不重发，直接按 stream_error 换号。
func TestForwardStreamSignatureErrorWithoutRectifierFailsOverDirectly(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{staticStreamBody(streamErrorSignatureFrame)}}
	svc := newStreamErrorEventForwardService(t, upstream, 0, nil)
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newStreamErrorEventThinkingParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Equal(t, 1, upstream.calls())
}

// 已交付语义内容后才出现签名错误：不能同号重发（会再拼一条 message_start），也不能换号。
func TestForwardStreamSignatureErrorAfterSemanticOutputDoesNotRetry(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{
		staticStreamBody(streamErrorSemanticPrefix + streamErrorSignatureFrame),
		staticStreamBody(streamErrorSuccessStream),
	}}
	svc := newStreamErrorEventForwardService(t, upstream, 0, apiKeySignatureRectifierOn())
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newStreamErrorEventThinkingParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.False(t, failoverErr.SafeToFailoverAfterWrite)
	require.Equal(t, 1, upstream.calls())
}

// 请求里根本没有 thinking 块时重发没有意义，不额外打上游。
func TestForwardStreamSignatureErrorWithoutThinkingBlocksSkipsRetry(t *testing.T) {
	upstream := &streamErrorEventUpstream{bodies: []func() io.ReadCloser{staticStreamBody(streamErrorSignatureFrame)}}
	svc := newStreamErrorEventForwardService(t, upstream, 0, apiKeySignatureRectifierOn())
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Equal(t, 1, upstream.calls())
}
