//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func streamRule(statusCode int, platform string) ErrorHandlingRule {
	return ErrorHandlingRule{
		ID:          "stream-rule",
		StatusCodes: []int{statusCode},
		Action:      ErrorHandlingActionFailover,
		Platforms:   []string{platform},
	}
}

func streamTestResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func requireRuleFailover(t *testing.T, err error, safeAfterWrite bool) *UpstreamFailoverError {
	t.Helper()
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "stream-rule", failoverErr.ErrorRuleID)
	require.Equal(t, safeAfterWrite, failoverErr.SafeToFailoverAfterWrite)
	return failoverErr
}

func TestGeminiMessagesStreamErrorHandlingRuleHitMissAndPostOutput(t *testing.T) {
	errorFrame := "data: {\"error\":{\"code\":503,\"message\":\"overloaded\"}}\n\n"

	t.Run("hit_before_output_suppresses_failure_frame", func(t *testing.T) {
		svc := newGeminiRuleService(t, nil, streamRule(http.StatusServiceUnavailable, PlatformGemini))
		c, rec := newGeminiRuleTestContext()
		_, err := svc.handleStreamingResponse(context.Background(), c, streamTestResponse(errorFrame), geminiRuleAccount(), time.Now(), "gemini", "gemini")

		requireRuleFailover(t, err, true)
		require.Empty(t, rec.Body.String())
	})

	t.Run("miss_writes_one_protocol_error", func(t *testing.T) {
		svc := newGeminiRuleService(t, nil, streamRule(http.StatusTooManyRequests, PlatformGemini))
		c, rec := newGeminiRuleTestContext()
		_, err := svc.handleStreamingResponse(context.Background(), c, streamTestResponse(errorFrame), geminiRuleAccount(), time.Now(), "gemini", "gemini")

		var failoverErr *UpstreamFailoverError
		require.Error(t, err)
		require.False(t, errors.As(err, &failoverErr))
		require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
		require.Equal(t, 1, strings.Count(rec.Body.String(), "overloaded"))
	})

	t.Run("hit_after_semantic_output_downgrades_to_passthrough", func(t *testing.T) {
		body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n" + errorFrame
		svc := newGeminiRuleService(t, nil, streamRule(http.StatusServiceUnavailable, PlatformGemini))
		c, rec := newGeminiRuleTestContext()
		_, err := svc.handleStreamingResponse(context.Background(), c, streamTestResponse(body), geminiRuleAccount(), time.Now(), "gemini", "gemini")

		failoverErr := requireRuleFailover(t, err, false)
		require.Equal(t, NextAccountStop, failoverErr.NextAccountAction)
		require.Equal(t, ErrorHandlingExhaustedActionPassthrough, failoverErr.ExhaustedAction)
		require.Contains(t, rec.Body.String(), "hello")
		require.NotContains(t, rec.Body.String(), "overloaded")
	})
}

func TestGeminiNativeStreamErrorHandlingRuleHitAndMiss(t *testing.T) {
	errorFrame := "data: {\"error\":{\"code\":503,\"message\":\"native overloaded\"}}\n\n"

	t.Run("hit", func(t *testing.T) {
		svc := newGeminiRuleService(t, nil, streamRule(http.StatusServiceUnavailable, PlatformGemini))
		c, rec := newGeminiRuleTestContext()
		_, err := svc.handleNativeStreamingResponse(context.Background(), c, streamTestResponse(errorFrame), geminiRuleAccount(), time.Now(), false, "gemini")

		requireRuleFailover(t, err, true)
		require.Empty(t, rec.Body.String())
	})

	t.Run("miss", func(t *testing.T) {
		svc := newGeminiRuleService(t, nil, streamRule(http.StatusTooManyRequests, PlatformGemini))
		c, rec := newGeminiRuleTestContext()
		_, err := svc.handleNativeStreamingResponse(context.Background(), c, streamTestResponse(errorFrame), geminiRuleAccount(), time.Now(), false, "gemini")

		var failoverErr *UpstreamFailoverError
		require.Error(t, err)
		require.False(t, errors.As(err, &failoverErr))
		require.Equal(t, 1, strings.Count(rec.Body.String(), "native overloaded"))
	})

	t.Run("structural_output_blocks_missing_terminal_failover", func(t *testing.T) {
		svc := newGeminiRuleService(t, nil, streamRule(http.StatusBadGateway, PlatformGemini))
		c, rec := newGeminiRuleTestContext()
		_, err := svc.handleNativeStreamingResponse(
			context.Background(), c, streamTestResponse("data: {\"candidates\":[]}\n\n"),
			geminiRuleAccount(), time.Now(), false, "gemini",
		)

		failoverErr := requireRuleFailover(t, err, false)
		require.True(t, failoverErr.SyntheticStatus)
		require.Contains(t, rec.Body.String(), `"candidates":[]`)
	})
}

func TestOpenAIChatCompletionsStreamErrorHandlingRuleHitMissAndPostOutput(t *testing.T) {
	failed := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"code\":\"internal_error\",\"message\":\"chat failed\"}}}\n\n"
	run := func(t *testing.T, svc *OpenAIGatewayService, body string) (*UpstreamFailoverError, string) {
		t.Helper()
		c, rec := newOpenAITransportErrTestContext()
		_, err := svc.handleChatStreamingResponse(streamTestResponse(body), c, openAIRuleAccount(), "gpt-test", "gpt-test", "gpt-test", time.Now(), 2)
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		return failoverErr, rec.Body.String()
	}

	t.Run("hit_before_output", func(t *testing.T) {
		svc := newOpenAIRuleService(t, nil, streamRule(http.StatusBadGateway, PlatformOpenAI))
		failoverErr, body := run(t, svc, failed)
		require.Equal(t, "stream-rule", failoverErr.ErrorRuleID)
		require.True(t, failoverErr.SafeToFailoverAfterWrite)
		require.Empty(t, body)
	})

	t.Run("miss_uses_builtin_failure", func(t *testing.T) {
		svc := newOpenAIRuleService(t, nil, streamRule(http.StatusTooManyRequests, PlatformOpenAI))
		failoverErr, _ := run(t, svc, failed)
		require.Empty(t, failoverErr.ErrorRuleID)
	})

	t.Run("hit_after_semantic_output", func(t *testing.T) {
		delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
		svc := newOpenAIRuleService(t, nil, streamRule(http.StatusBadGateway, PlatformOpenAI))
		failoverErr, body := run(t, svc, delta+failed)
		require.Equal(t, "stream-rule", failoverErr.ErrorRuleID)
		require.False(t, failoverErr.SafeToFailoverAfterWrite)
		require.Equal(t, NextAccountStop, failoverErr.NextAccountAction)
		require.Contains(t, body, "hello")
		require.NotContains(t, body, "chat failed")
	})
}

func TestOpenAIResponsesStreamErrorHandlingRuleHitAndMiss(t *testing.T) {
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"code\":\"internal_error\",\"message\":\"responses failed\"}}}\n\n"
	run := func(t *testing.T, rule ErrorHandlingRule) (*UpstreamFailoverError, string) {
		t.Helper()
		svc := newOpenAIRuleService(t, nil, rule)
		svc.toolCorrector = NewCodexToolCorrector()
		c, rec := newOpenAITransportErrTestContext()
		_, err := svc.handleStreamingResponse(context.Background(), streamTestResponse(failed), c, openAIRuleAccount(), time.Now(), "gpt-test", "gpt-test")
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		return failoverErr, rec.Body.String()
	}

	t.Run("hit", func(t *testing.T) {
		failoverErr, body := run(t, streamRule(http.StatusBadGateway, PlatformOpenAI))
		require.Equal(t, "stream-rule", failoverErr.ErrorRuleID)
		require.True(t, failoverErr.SafeToFailoverAfterWrite)
		require.Empty(t, body)
	})

	t.Run("miss", func(t *testing.T) {
		failoverErr, _ := run(t, streamRule(http.StatusTooManyRequests, PlatformOpenAI))
		require.Empty(t, failoverErr.ErrorRuleID)
	})
}

func TestAntigravityUpstreamStreamErrorHandlingRuleHitAndMiss(t *testing.T) {
	errorFrame := "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":503,\"message\":\"upstream overloaded\"}}\n\n"
	run := func(t *testing.T, rule ErrorHandlingRule) (error, string) {
		t.Helper()
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, rule)}
		c, rec := newAntigravityRuleTestContext()
		_, err := svc.streamUpstreamResponseWithRules(context.Background(), c, streamTestResponse(errorFrame), antigravityRuleAccount(), time.Now(), "claude-test")
		return err, rec.Body.String()
	}

	t.Run("hit_suppresses_complete_failure_frame", func(t *testing.T) {
		err, body := run(t, streamRule(http.StatusServiceUnavailable, PlatformAntigravity))
		requireRuleFailover(t, err, true)
		require.Empty(t, body)
	})

	t.Run("miss_forwards_complete_failure_frame_once", func(t *testing.T) {
		err, body := run(t, streamRule(http.StatusTooManyRequests, PlatformAntigravity))
		var failoverErr *UpstreamFailoverError
		require.Error(t, err)
		require.False(t, errors.As(err, &failoverErr))
		require.Equal(t, 1, strings.Count(body, "event: error"))
		require.Equal(t, 1, strings.Count(body, "upstream overloaded"))
	})
}

func TestAntigravityCompatStreamsApplyEmbeddedErrorRules(t *testing.T) {
	errorFrame := "data: {\"error\":{\"code\":503,\"message\":\"compat overloaded\"}}\n\n"
	tests := []struct {
		name string
		call func(*AntigravityGatewayService, *http.Response, *gin.Context, antigravityStreamRuleOptions) (*antigravityStreamResult, error)
	}{
		{
			name: "chat_completions",
			call: func(svc *AntigravityGatewayService, resp *http.Response, c *gin.Context, options antigravityStreamRuleOptions) (*antigravityStreamResult, error) {
				return svc.handleChatCompletionsStreamingFromAntigravity(c, resp, time.Now(), "claude-test", false, options)
			},
		},
		{
			name: "responses",
			call: func(svc *AntigravityGatewayService, resp *http.Response, c *gin.Context, options antigravityStreamRuleOptions) (*antigravityStreamResult, error) {
				return svc.handleResponsesStreamingFromAntigravity(c, resp, time.Now(), "claude-test", options)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"_hit", func(t *testing.T) {
			svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusServiceUnavailable, PlatformAntigravity))}
			c, rec := newAntigravityRuleTestContext()
			options := antigravityStreamRuleOptions{ctx: context.Background(), account: antigravityRuleAccount(), reqModel: "claude-test"}

			_, err := tt.call(svc, streamTestResponse(errorFrame), c, options)

			requireRuleFailover(t, err, true)
			require.Empty(t, rec.Body.String())
		})

		t.Run(tt.name+"_miss", func(t *testing.T) {
			svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusTooManyRequests, PlatformAntigravity))}
			c, rec := newAntigravityRuleTestContext()
			options := antigravityStreamRuleOptions{ctx: context.Background(), account: antigravityRuleAccount(), reqModel: "claude-test"}

			_, err := tt.call(svc, streamTestResponse(errorFrame), c, options)

			var failoverErr *UpstreamFailoverError
			require.Error(t, err)
			require.False(t, errors.As(err, &failoverErr))
			require.Equal(t, 1, strings.Count(rec.Body.String(), "compat overloaded"))
		})
	}
}

func TestAntigravityUpstreamSyntheticStreamFailures(t *testing.T) {
	run := func(t *testing.T, svc *AntigravityGatewayService, resp *http.Response) (*gin.Context, string, error) {
		t.Helper()
		c, rec := newAntigravityRuleTestContext()
		_, err := svc.streamUpstreamResponseWithRules(context.Background(), c, resp, antigravityRuleAccount(), time.Now(), "claude-test")
		return c, rec.Body.String(), err
	}
	requireSynthetic := func(t *testing.T, c *gin.Context, err error, safeAfterWrite bool) {
		t.Helper()
		failoverErr := requireRuleFailover(t, err, safeAfterWrite)
		require.True(t, failoverErr.SyntheticStatus)
		_, hasStatus := c.Get(OpsUpstreamStatusCodeKey)
		require.False(t, hasStatus)
	}

	t.Run("missing_terminal_after_ping_remains_failover_safe", func(t *testing.T) {
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusBadGateway, PlatformAntigravity))}
		c, body, err := run(t, svc, streamTestResponse("event: ping\ndata: {\"type\":\"ping\"}\n\n"))

		requireSynthetic(t, c, err, true)
		require.Contains(t, body, "event: ping")
		require.NotContains(t, body, "event: error")
	})

	t.Run("structural_output_blocks_failover", func(t *testing.T) {
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusBadGateway, PlatformAntigravity))}
		c, body, err := run(t, svc, streamTestResponse("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n"))

		requireSynthetic(t, c, err, false)
		require.Contains(t, body, "message_start")
		require.NotContains(t, body, "event: error")
	})

	t.Run("read_error_matches_synthetic_502", func(t *testing.T) {
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusBadGateway, PlatformAntigravity))}
		resp := streamTestResponse("")
		resp.Body = &antigravityCompatErrorReader{err: io.ErrUnexpectedEOF}
		c, body, err := run(t, svc, resp)

		requireSynthetic(t, c, err, true)
		require.Empty(t, body)
	})

	t.Run("missing_rule_writes_one_terminal_error", func(t *testing.T) {
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusTooManyRequests, PlatformAntigravity))}
		_, body, err := run(t, svc, streamTestResponse(""))

		var failoverErr *UpstreamFailoverError
		require.Error(t, err)
		require.False(t, errors.As(err, &failoverErr))
		require.Equal(t, 1, strings.Count(body, "event: error"))
		require.Equal(t, 1, strings.Count(body, "ended before a terminal event"))
	})

	t.Run("timeout_matches_synthetic_502", func(t *testing.T) {
		svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusBadGateway, PlatformAntigravity))}
		svc.settingService.cfg.Gateway.StreamDataIntervalTimeout = 1
		reader, writer := io.Pipe()
		resp := streamTestResponse("")
		resp.Body = reader

		c, body, err := run(t, svc, resp)
		require.NoError(t, writer.Close())
		require.NoError(t, reader.Close())

		requireSynthetic(t, c, err, true)
		require.Empty(t, body)
	})
}

func TestAntigravityConvertedStructuralOutputBlocksFailover(t *testing.T) {
	options := antigravityStreamRuleOptions{
		ctx: context.Background(), account: antigravityRuleAccount(), reqModel: "claude-test",
	}
	tests := []struct {
		name string
		call func(*AntigravityGatewayService, *gin.Context, *http.Response) (*antigravityStreamResult, error)
	}{
		{
			name: "gemini",
			call: func(svc *AntigravityGatewayService, c *gin.Context, resp *http.Response) (*antigravityStreamResult, error) {
				return svc.handleGeminiStreamingResponse(c, resp, time.Now(), options)
			},
		},
		{
			name: "claude",
			call: func(svc *AntigravityGatewayService, c *gin.Context, resp *http.Response) (*antigravityStreamResult, error) {
				return svc.handleClaudeStreamingResponse(c, resp, time.Now(), "claude-test", options)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &AntigravityGatewayService{settingService: newAntigravityRuleSettingService(t, streamRule(http.StatusBadGateway, PlatformAntigravity))}
			c, rec := newAntigravityRuleTestContext()
			resp := streamTestResponse("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[]}}]}}\n\n")

			_, err := tt.call(svc, c, resp)

			failoverErr := requireRuleFailover(t, err, false)
			require.True(t, failoverErr.SyntheticStatus)
			require.NotEmpty(t, rec.Body.String())
		})
	}
}

func TestAntigravityUpstreamStreamAllowsNilSettingService(t *testing.T) {
	svc := &AntigravityGatewayService{}
	c, rec := newAntigravityRuleTestContext()
	resp := streamTestResponse("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	result, err := svc.streamUpstreamResponseWithRules(context.Background(), c, resp, nil, time.Now(), "claude-test")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "message_stop")
}

func TestAntigravityClaudeStreamAllowsNilSettingService(t *testing.T) {
	svc := &AntigravityGatewayService{}
	c, rec := newAntigravityRuleTestContext()
	resp := streamTestResponse(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}}` + "\n\n")

	result, err := svc.handleClaudeStreamingResponse(c, resp, time.Now(), "claude-test")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "message_stop")
}
