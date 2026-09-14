package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// #245：透传路径共有三个「上游把流搞断」的终止点，此前只有 missing_terminal_event
// 挂了规则钩子；read_error 与 interval_timeout 直接裸返回 error，调用方
// errors.As(err, &failoverErr) 不成立，整个 failover 块（同号重试 / 换号 / 规则
// 动作）被跳过。生产实锤见 crs15 ops_error_logs id=1224773
// （trace a3b0736f694dfdac，读 body 623s 后 unexpected EOF，零规则命中）。
//
// 这批用例锁两件事：
//   - 首字前断流（客户端零字节）必须进引擎，命中规则后可干净换号 / 重试；
//   - 首字后断流、客户端已断开、未命中规则时行为与修复前逐字节一致。

// streamFailureUpstream 让每次 attempt 注入自定义 Body，用来模拟读流报错和
// 「读到一半静默」——sequencedHTTPUpstream 只能给字符串 body，模拟不出 read_error。
type streamFailureUpstreamResponse struct {
	status int
	body   func() io.ReadCloser
}

type streamFailureUpstream struct {
	responses []streamFailureUpstreamResponse
	calls     int
}

func (u *streamFailureUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	idx := u.calls
	if idx >= len(u.responses) {
		idx = len(u.responses) - 1
	}
	u.calls++
	r := u.responses[idx]
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       r.body(),
	}, nil
}

func (u *streamFailureUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func readErrorBody(payload string) func() io.ReadCloser {
	return func() io.ReadCloser {
		return &streamReadCloser{payload: []byte(payload), err: io.ErrUnexpectedEOF}
	}
}

func stringBody(payload string) func() io.ReadCloser {
	return func() io.ReadCloser { return io.NopCloser(strings.NewReader(payload)) }
}

// streamFailureRule 只按合成的虚拟 502 + 归一文案匹配，不含上游原文——与线上
// 「上游渠道报错切号」（status_codes 含 502、keywords 为空）的配置形态等价。
func streamFailureRule(action string, keyword string, retries int) ErrorHandlingRule {
	return ErrorHandlingRule{
		ID: "stream-failure", StatusCodes: []int{http.StatusBadGateway},
		Keywords: []string{keyword}, Action: action,
		RetryCount: errorHandlingIntPtr(retries), ExhaustedAction: ErrorHandlingExhaustedActionDefault,
	}
}

func TestPassthroughStreamReadErrorFailsOverThroughVirtual502(t *testing.T) {
	upstream := &streamFailureUpstream{responses: []streamFailureUpstreamResponse{
		{status: 200, body: readErrorBody("")},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamFailureRule(ErrorHandlingActionFailover, "stream read error", 0)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "首字前读流报错命中 failover 规则时必须返回可换号的类型化错误")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite, "客户端零字节，换号是安全的")
	require.Equal(t, 1, upstream.calls, "failover 是终态，service 层不应再打上游")
	require.Empty(t, recorder.Body.String(), "换号前不得向客户端写出任何内容")

	// 合成的 502 不是真实上游状态，不能落进 ops_error_logs 顶层列——那一列为
	// NULL 正是「传输层失败、没有 HTTP 响应」的判定依据。
	_, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, ok, "合成状态码只能参与匹配，不得污染 ops_error_logs 顶层列")
}

func TestPassthroughStreamReadErrorRetriesInPlaceThenSucceeds(t *testing.T) {
	upstream := &streamFailureUpstream{responses: []streamFailureUpstreamResponse{
		{status: 200, body: readErrorBody("")},
		{status: 200, body: stringBody(streamRuleSuccess)},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamFailureRule(ErrorHandlingActionRetry, "stream read error", 1)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.calls)
	require.Contains(t, recorder.Body.String(), "event: message_stop")
	require.NotContains(t, recorder.Body.String(), "stream read error")
}

// 首字后断流重试会腐化流（双 message_start），必须维持裸返回、不进引擎。
func TestPassthroughStreamReadErrorAfterSemanticOutputDoesNotFailOver(t *testing.T) {
	semantic := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n"
	upstream := &streamFailureUpstream{responses: []streamFailureUpstreamResponse{
		{status: 200, body: readErrorBody(semantic)},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamFailureRule(ErrorHandlingActionFailover, "stream read error", 0)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.Error(t, err)
	require.False(t, errors.As(err, &failoverErr), "已交付语义内容后不得换号")
	require.Equal(t, 1, upstream.calls)
	require.Contains(t, recorder.Body.String(), "event: message_start")
}

// 规则未命中时行为与修复前完全一致：裸 error，不换号。
func TestPassthroughStreamReadErrorUnmatchedRuleKeepsBareError(t *testing.T) {
	upstream := &streamFailureUpstream{responses: []streamFailureUpstreamResponse{
		{status: 200, body: readErrorBody("")},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamFailureRule(ErrorHandlingActionFailover, "missing terminal event", 0)},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorContains(t, err, "stream read error")
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, 1, upstream.calls)
}

// 客户端先走的场景由 read_error 分支已有的前置 return 挡住，重试只会为没有接收方
// 的请求多烧一次上游开销（#5148）。
func TestPassthroughStreamReadErrorAfterClientDisconnectDoesNotRetry(t *testing.T) {
	upstream := &streamFailureUpstream{responses: []streamFailureUpstreamResponse{
		{status: 200, body: readErrorBody(`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}` + "\n\n")},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamFailureRule(ErrorHandlingActionRetry, "stream read error", 1)},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	require.Error(t, err)
	require.NotNil(t, result, "已观测到的 usage 必须在客户端断开后保留用于计费")
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 1, upstream.calls, "客户端已断开，不得再打上游")
}

func TestPassthroughStreamIntervalTimeoutFailsOverThroughVirtual502(t *testing.T) {
	upstream := &streamFailureUpstream{responses: []streamFailureUpstreamResponse{{
		status: 200,
		// 永不返回数据也永不关闭，直到间隔超时触发。
		body: func() io.ReadCloser {
			reader, _ := io.Pipe()
			return reader
		},
	}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamFailureRule(ErrorHandlingActionFailover, "stream data interval timeout", 0)},
	})
	svc.cfg.Gateway.StreamDataIntervalTimeout = 1

	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "首字前间隔超时命中 failover 规则时必须可换号")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Equal(t, 1, upstream.calls)
	require.NotContains(t, recorder.Body.String(), "event: error")

	_, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, ok, "合成状态码只能参与匹配，不得污染 ops_error_logs 顶层列")
}

// 直接驱动 service 层，锁住「引擎确实被调用过」——返回的 match 必须带
// synthetic=true，这是 ops 顶层状态码保持 NULL 的前提。
func TestPassthroughStreamReadErrorReturnsSyntheticRuleMatch(t *testing.T) {
	svc := newErrorHandlingRulePassthroughService(t, &streamFailureUpstream{}, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamFailureRule(ErrorHandlingActionFailover, "stream read error", 0)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &streamReadCloser{err: io.ErrUnexpectedEOF},
	}
	var tracker errorHandlingRuleTracker

	result, match, err := svc.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		context.Background(), resp, c, newErrorHandlingRulePassthroughAccount(), time.Now(), "claude-sonnet-4-5", &tracker, nil, 1,
	)

	require.NoError(t, err, "命中规则时以 match 表达终态，不再返回裸 error")
	require.NotNil(t, result)
	require.NotNil(t, match, "引擎必须被调用并返回命中")
	require.True(t, match.synthetic, "合成的 502 必须标记为 synthetic")
	require.False(t, match.semanticEventForwarded)
	require.Equal(t, http.StatusBadGateway, match.statusCode)
	require.Equal(t, ErrorHandlingActionFailover, match.decision.EffectiveAction)
	require.Empty(t, recorder.Body.String())
}
