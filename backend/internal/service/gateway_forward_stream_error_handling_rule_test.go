package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func convertedStreamRule(action string, retries int) ErrorHandlingRule {
	return ErrorHandlingRule{
		ID:          "converted-stream-interruption",
		StatusCodes: []int{http.StatusBadGateway},
		Keywords:    []string{"missing terminal event"},
		Platforms:   []string{PlatformAnthropic},
		Action:      action,
		RetryCount:  errorHandlingIntPtr(retries),
	}
}

// newConvertedStreamForwardService 与 newErrorHandlingRuleForwardService 等价，
// 但接受任意 HTTPUpstream（需要能给出 io.Pipe 驱动的慢 body 来复现 keepalive
// 心跳），并允许配置 keepalive 间隔。
func newConvertedStreamForwardService(t *testing.T, upstream HTTPUpstream, keepaliveInterval int, ruleSettings *ErrorHandlingRuleSettings) *GatewayService {
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
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), ruleSettings))
	return svc
}

func TestConvertedStreamMissingTerminalRuleFailsOverBeforeOutput(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: http.StatusOK, body: ""}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{convertedStreamRule(ErrorHandlingActionFailover, 0)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, []byte(anthropicStreamMissingTerminalBody), failoverErr.ResponseBody)
	require.Equal(t, "converted-stream-interruption", failoverErr.ErrorRuleID)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Empty(t, recorder.Body.String())
	require.Equal(t, 1, upstream.calls)

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "error_handling_rule_failover", events[0].Kind)
	require.False(t, events[0].Passthrough)
}

func TestConvertedStreamMissingTerminalRetryRuleDowngradesToFailover(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: http.StatusOK, body: ""}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{convertedStreamRule(ErrorHandlingActionRetry, 1)},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "converted-stream-interruption", failoverErr.ErrorRuleID)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.Equal(t, 1, upstream.calls, "转换路径已离开 HTTP 重试循环，不能原账号重放")
}

func TestConvertedStreamMissingTerminalRulePreservesWrittenStreamAndPartialUsage(t *testing.T) {
	upstreamBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_partial","model":"claude-sonnet-4-5","usage":{"input_tokens":9,"cache_read_input_tokens":2}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":3}}`,
		"",
		"",
	}, "\n")
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: http.StatusOK, body: upstreamBody}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{convertedStreamRule(ErrorHandlingActionFailover, 0)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	require.ErrorContains(t, err, anthropicStreamMissingTerminalMessage)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "已写出流内容后不得换号")
	require.NotNil(t, result)
	require.True(t, result.StreamIncomplete)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.CacheReadInputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), "event: message_start")
	// writer 守卫在规则判定前就拦住了，所以只应留下纯观测的 stream_failure，
	// 不应有任何 error_handling_rule_* 记录。
	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, opsUpstreamErrorKindStreamFailure, events[0].Kind)
	require.Equal(t, string(streamFailureMissingTerminal), events[0].Reason)
	require.False(t, events[0].Passthrough)
}

func TestConvertedStreamMissingTerminalPassthroughRuleWritesSyntheticError(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: http.StatusOK, body: ""}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{convertedStreamRule(ErrorHandlingActionPassthrough, 0)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	require.Nil(t, result)
	require.ErrorContains(t, err, "error handling rule passthrough")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, "event: error\ndata: "+anthropicStreamMissingTerminalBody+"\n\n", recorder.Body.String())

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "error_handling_rule_passthrough", events[0].Kind)
}

// TestConvertedStreamCleanEOFAfterLocalKeepaliveCanFailOver 是 #204 最关键的回归。
//
// stream_keepalive_interval 默认 10s 且默认开启，issue 生产样本的静默窗口正好是
// 10.183s —— 也就是说流中断前本机必然已经写出过一帧 event: ping。若守卫用裸的
// c.Writer.Size() == writerSizeBeforeStream，这个修复在它自己的生产样本上就是
// no-op。这里用 io.Pipe 让上游静默超过一个心跳周期再干净关流，锁死「心跳字节
// 不算已交付内容」的口径，并要求显式放行 handler 侧的第二道 writer 闸门。
// 对应的透传侧用例是 TestPassthroughStreamCleanEOFAfterLocalKeepaliveCanFailOver。
func TestConvertedStreamCleanEOFAfterLocalKeepaliveCanFailOver(t *testing.T) {
	reader, writer := io.Pipe()
	upstream := &singleResponseHTTPUpstream{status: http.StatusOK, body: reader}
	svc := newConvertedStreamForwardService(t, upstream, 1, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{convertedStreamRule(ErrorHandlingActionFailover, 0)},
	})
	c, recorder := newErrorHandlingRuleTestContextWithRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(1100 * time.Millisecond)
		_ = writer.Close()
	}()

	result, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))
	<-done

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "converted-stream-interruption", failoverErr.ErrorRuleID)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	// handler 的 gatewayForwardMayFailover 第一道闸门会因心跳字节而失效，
	// 必须靠这个标记放行，否则仍会落到 handleFailoverExhausted 不换号。
	require.True(t, failoverErr.SafeToFailoverAfterWrite)

	// 客户端只收到过非语义的心跳，没有任何语义帧 —— 换号后拼接下一账号的
	// message_start 是协议合规的。
	require.Contains(t, recorder.Body.String(), "event: ping")
	require.NotContains(t, recorder.Body.String(), "event: message_start")
	require.NotContains(t, recorder.Body.String(), "event: error")

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "error_handling_rule_failover", events[0].Kind)
}

// TestConvertedStreamMissingTerminalWithoutRuleRecordsOpsCause 覆盖缺口 2：
// 规则未命中时转换路径此前完全不记，ops_error_logs.upstream_errors 恒为 NULL，
// 线上无从区分「引擎没命中」和「引擎没跑」。passthrough 必须是 false，否则会
// 谎报成透传路径的样本。对应的透传侧用例是
// TestPassthroughMissingTerminalRecordsOpsCause。
func TestConvertedStreamMissingTerminalWithoutRuleRecordsOpsCause(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: http.StatusOK, body: ""}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{Enabled: false})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	ctx, logs := newStreamFailureLogObserver(t)

	_, err := svc.Forward(ctx, c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, anthropicStreamMissingTerminalMessage)

	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "规则未启用时不得换号")

	msg, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok, "转换路径必须写 upstream_error_message，否则线上恒为 NULL")
	require.Equal(t, anthropicStreamMissingTerminalMessage, msg)

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, opsUpstreamErrorKindStreamFailure, events[0].Kind)
	require.Equal(t, string(streamFailureMissingTerminal), events[0].Reason)
	require.Equal(t, streamFailureScopeBeforeFirstToken, events[0].Scope)
	require.False(t, events[0].Passthrough, "转换路径不能谎报成 passthrough")
	// wire 上游状态确实是 200，不能合成 5xx 污染上游状态维度。
	require.Zero(t, events[0].UpstreamStatusCode)

	requireStreamFailureLog(t, logs, streamFailureMissingTerminal, streamFailureScopeBeforeFirstToken, 701, "claude-sonnet-4-5")
}

// TestConvertedStreamMissingTerminalRuleMatchDoesNotStackStreamFailure 保证命中
// 规则时只产生一条 error_handling_rule_*，不叠加纯观测的 stream_failure ——
// 与透传侧「命中即 return、不再 recordStreamFailureCause」的顺序语义一致。
func TestConvertedStreamMissingTerminalRuleMatchDoesNotStackStreamFailure(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: http.StatusOK, body: ""}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{convertedStreamRule(ErrorHandlingActionFailover, 0)},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	ctx, logs := newStreamFailureLogObserver(t)

	_, err := svc.Forward(ctx, c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleStreamParsed(t))

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "error_handling_rule_failover", events[0].Kind)
	require.Empty(t, logs.FilterMessage("gateway.stream_failure").All(), "命中规则时不应再落 stream_failure")
}

func TestStreamOnlyWroteKeepalive(t *testing.T) {
	t.Run("尚未写入任何字节", func(t *testing.T) {
		c, _ := newErrorHandlingRuleTestContextWithRecorder()
		// gin 的 Size() 在首次写入前是 noWritten(-1)，两侧 clamp 后应相等。
		require.Equal(t, -1, c.Writer.Size())
		require.True(t, streamOnlyWroteKeepalive(c, c.Writer.Size(), 0))
	})

	t.Run("只写过心跳", func(t *testing.T) {
		c, _ := newErrorHandlingRuleTestContextWithRecorder()
		before := c.Writer.Size()
		ping := "event: ping\ndata: {\"type\": \"ping\"}\n\n"
		n, err := c.Writer.WriteString(ping)
		require.NoError(t, err)
		// 裸比较在这里就会失败 —— 这正是 #204 的 no-op 成因。
		require.NotEqual(t, before, c.Writer.Size())
		require.True(t, streamOnlyWroteKeepalive(c, before, n))
	})

	t.Run("写过语义内容", func(t *testing.T) {
		c, _ := newErrorHandlingRuleTestContextWithRecorder()
		before := c.Writer.Size()
		ping := "event: ping\ndata: {\"type\": \"ping\"}\n\n"
		n, err := c.Writer.WriteString(ping)
		require.NoError(t, err)
		_, err = c.Writer.WriteString("event: message_start\ndata: {}\n\n")
		require.NoError(t, err)
		require.False(t, streamOnlyWroteKeepalive(c, before, n))
	})

	t.Run("nil context", func(t *testing.T) {
		require.False(t, streamOnlyWroteKeepalive(nil, 0, 0))
	})
}
