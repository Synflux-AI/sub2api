package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ForwardUpstream 使用 base_url + /v1/messages + 双 header 认证透传上游 Claude 请求
func (s *AntigravityGatewayService) ForwardUpstream(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	beginUpstreamResponseModelObservation(c)
	startTime := time.Now()
	sessionID := getSessionID(c)
	prefix := logPrefix(sessionID, account.Name)

	// 获取上游配置
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	if baseURL == "" || apiKey == "" {
		return nil, fmt.Errorf("upstream account missing base_url or api_key")
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	// 解析请求获取模型信息
	var claudeReq antigravity.ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		return nil, fmt.Errorf("parse claude request: %w", err)
	}
	if strings.TrimSpace(claudeReq.Model) == "" {
		return nil, fmt.Errorf("missing model")
	}
	originalModel := claudeReq.Model

	// 构建上游请求 URL
	upstreamURL := baseURL + "/v1/messages"

	// 能力维度 sanitize：Anthropic-compatible 上游透传路径也需要保证 body↔beta header
	// 对称。客户端 anthropic-beta header 不含 context-management-2025-06-27 但 body 带
	// context_management 时 strip，与 Anthropic 直连 / Bedrock / Vertex 路径保持一致。
	clientBeta := c.GetHeader("anthropic-beta")
	if sanitized, changed := sanitizeAnthropicBodyForBetaTokens(body, clientBeta); changed {
		body = sanitized
	}

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey) // Claude API 兼容

	// 透传 Claude 相关 headers
	if v := c.GetHeader("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
	}
	if v := clientBeta; v != "" {
		req.Header.Set("anthropic-beta", v)
	}

	// 链路关联 ID（账号开启 trace_id_passthrough 时才注入）。
	// 该路径账号类型恒为中转实例，但仍受开关约束，不硬编码注入。
	injectTraceHeader(ctx, req, account)

	// 代理 URL
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	// 发送请求
	account.ApplyCustomHeaders(req)
	resp, err := timedUpstreamDo(c, s.httpUpstream, req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "%s upstream request failed: %v", prefix, err)
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		if ctx.Err() == nil {
			if ruleErr := s.antigravityTransportErrorRuleOverride(ctx, c, account, safeErr); ruleErr != nil {
				return nil, ruleErr
			}
		}
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 处理错误响应
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)

		// 429 错误时标记账号限流
		if resp.StatusCode == http.StatusTooManyRequests {
			s.handleUpstreamError(ctx, prefix, account, resp.StatusCode, resp.Header, respBody, originalModel, 0, "", false)
		}
		if failoverErr, handled := s.antigravityErrorHandlingRuleOverride(ctx, c, antigravityErrorHandlingRuleInput{
			Account:             account,
			StatusCode:          resp.StatusCode,
			Header:              resp.Header,
			Body:                respBody,
			ReqModel:            originalModel,
			BuiltinWillFailover: false,
		}); handled {
			if resp.StatusCode != http.StatusTooManyRequests {
				s.handleUpstreamError(ctx, prefix, account, resp.StatusCode, resp.Header, respBody, originalModel, 0, "", false)
			}
			return nil, failoverErr
		}

		// 透传上游错误
		c.Header("Content-Type", resp.Header.Get("Content-Type"))
		c.Status(resp.StatusCode)
		_, _ = c.Writer.Write(respBody)

		return &ForwardResult{
			Model: originalModel,
		}, nil
	}

	// 处理成功响应（流式/非流式）
	var usage *ClaudeUsage
	var firstTokenMs *int
	var clientDisconnect bool

	if claudeReq.Stream {
		// 流式响应：透传
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)

		streamRes, err := s.streamUpstreamResponseWithRules(ctx, c, resp, account, startTime, originalModel)
		if err != nil {
			return nil, err
		}
		usage = streamRes.usage
		firstTokenMs = streamRes.firstTokenMs
		clientDisconnect = streamRes.clientDisconnect
	} else {
		// 非流式响应：直接透传
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read upstream response: %w", err)
		}

		// 提取 usage
		upstreamResponseModelObserverFromContext(c).ObserveAnthropic(respBody)
		usage = s.extractClaudeUsage(respBody)

		c.Header("Content-Type", resp.Header.Get("Content-Type"))
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write(respBody)
	}

	// 构建计费结果
	duration := time.Since(startTime)
	logger.LegacyPrintf("service.antigravity_gateway", "%s status=success duration_ms=%d", prefix, duration.Milliseconds())

	return &ForwardResult{
		Model:                         originalModel,
		UpstreamResponseModel:         observedUpstreamResponseModel(c),
		UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		Stream:                        claudeReq.Stream,
		Duration:                      duration,
		FirstTokenMs:                  firstTokenMs,
		ClientDisconnect:              clientDisconnect,
		UpstreamHeaders:               resp.Header,
		Usage: ClaudeUsage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
		},
	}, nil
}

// streamUpstreamResponse 保留旧的测试入口；生产转发使用带规则上下文的版本。
func (s *AntigravityGatewayService) streamUpstreamResponse(c *gin.Context, resp *http.Response, startTime time.Time) *antigravityStreamResult {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	result, _ := s.streamUpstreamResponseWithRules(ctx, c, resp, nil, startTime, "")
	return result
}

// streamUpstreamResponseWithRules 透传上游 SSE 流、执行流内错误规则并提取 Claude usage。
func (s *AntigravityGatewayService) streamUpstreamResponseWithRules(ctx context.Context, c *gin.Context, resp *http.Response, account *Account, startTime time.Time, reqModel string) (*antigravityStreamResult, error) {
	usage := &ClaudeUsage{}
	var firstTokenMs *int
	sawTerminalEvent := false
	pendingEventLine := ""

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.settingService != nil && s.settingService.cfg != nil && s.settingService.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.settingService.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)

	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	go func() {
		defer close(events)
		for scanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}()
	defer close(done)

	streamInterval := time.Duration(0)
	if s.settingService != nil && s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.settingService.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	// 下游 keepalive：防止代理/Cloudflare Tunnel 因连接空闲而断开
	keepaliveInterval := time.Duration(0)
	if s.settingService != nil && s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.settingService.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}
	lastDataAt := time.Now()

	flusher, _ := c.Writer.(http.Flusher)
	cw := newAntigravityClientWriter(c.Writer, flusher, "antigravity upstream")
	errorEventSent := false
	sendErrorEvent := func(reason string) {
		if errorEventSent || cw.Disconnected() {
			return
		}
		errorEventSent = true
		reason = sanitizeUpstreamErrorMessage(strings.TrimSpace(reason))
		if reason == "" {
			reason = "Upstream stream failed"
		}
		payload, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "upstream_error", "message": reason},
		})
		cw.Fprintf("event: error\ndata: %s\n\n", payload)
		MarkResponseCommitted(c)
	}
	result := func(clientDisconnect bool) *antigravityStreamResult {
		return &antigravityStreamResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnect}
	}
	failoverUnsafeOutputForwarded := false

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if !sawTerminalEvent && ctx.Err() == nil && !cw.Disconnected() {
					message := "Antigravity upstream stream ended before a terminal event"
					if ruleErr := s.antigravityStreamErrorHandlingRuleOverride(
						ctx, c, account, resp.Header, 0, nil, reqModel, message, true, failoverUnsafeOutputForwarded,
					); ruleErr != nil {
						return result(false), ruleErr
					}
					sendErrorEvent(message)
					return result(false), errors.New("stream usage incomplete: missing terminal event")
				}
				return result(cw.Disconnected()), nil
			}
			if ev.err != nil {
				if disconnect, handled := handleStreamReadError(ev.err, cw.Disconnected(), "antigravity upstream"); handled {
					return result(disconnect), nil
				}
				if ruleErr := s.antigravityStreamErrorHandlingRuleOverride(
					ctx, c, account, resp.Header, 0, nil, reqModel,
					"Antigravity upstream stream read error: "+ev.err.Error(), true, failoverUnsafeOutputForwarded,
				); ruleErr != nil {
					return result(false), ruleErr
				}
				logger.LegacyPrintf("service.antigravity_gateway", "Stream read error (antigravity upstream): %v", ev.err)
				sendErrorEvent("stream_read_error")
				return result(false), fmt.Errorf("stream read error: %w", ev.err)
			}

			lastDataAt = time.Now()

			line := ev.line
			if eventType, ok := extractOpenAISSEEventLine(line); ok {
				pendingEventLine = "event: " + strings.TrimSpace(eventType)
				continue
			}
			if data, ok := extractAnthropicSSEDataLine(line); ok {
				payload := []byte(strings.TrimSpace(data))
				upstreamResponseModelObserverFromContext(c).ObserveAnthropic(payload)
				eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
				if eventType == "message_stop" {
					sawTerminalEvent = true
				}
				if statusCode, errorBody, message, isError := antigravityStreamErrorPayload(payload); isError {
					if ruleErr := s.antigravityStreamErrorHandlingRuleOverride(
						ctx, c, account, resp.Header, statusCode, errorBody, reqModel, message, false, failoverUnsafeOutputForwarded,
					); ruleErr != nil {
						return result(false), ruleErr
					}
					if pendingEventLine != "" {
						cw.Fprintf("%s\n", pendingEventLine)
					}
					cw.Fprintf("%s\n\n", line)
					MarkResponseCommitted(c)
					return result(false), fmt.Errorf("upstream response failed: %s", message)
				}
				if eventType != "ping" {
					failoverUnsafeOutputForwarded = true
				}
			}

			// 记录首 token 时间
			if firstTokenMs == nil && len(line) > 0 {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}

			// 尝试从 message_delta 或 message_stop 事件提取 usage
			s.extractSSEUsage(line, usage)

			// 透传行
			if pendingEventLine != "" {
				cw.Fprintf("%s\n", pendingEventLine)
				if !strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(pendingEventLine, "event:")), "ping") {
					failoverUnsafeOutputForwarded = true
				}
				pendingEventLine = ""
			}
			cw.Fprintf("%s\n", line)
			if _, isDataLine := extractAnthropicSSEDataLine(line); !isDataLine {
				trimmedLine := strings.TrimSpace(line)
				if trimmedLine != "" && !strings.HasPrefix(trimmedLine, ":") {
					failoverUnsafeOutputForwarded = true
				}
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if cw.Disconnected() {
				logger.LegacyPrintf("service.antigravity_gateway", "Upstream timeout after client disconnect (antigravity upstream), returning collected usage")
				return result(true), nil
			}
			logger.LegacyPrintf("service.antigravity_gateway", "Stream data interval timeout (antigravity upstream)")
			if ruleErr := s.antigravityStreamErrorHandlingRuleOverride(
				ctx, c, account, resp.Header, 0, nil, reqModel,
				"Antigravity upstream stream data interval timeout", true, failoverUnsafeOutputForwarded,
			); ruleErr != nil {
				return result(false), ruleErr
			}
			sendErrorEvent("stream_timeout")
			return result(false), fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if cw.Disconnected() {
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				continue
			}
			// SSE ping 事件：Anthropic 原生格式，客户端会正确处理，
			// 同时保持连接活跃防止 Cloudflare Tunnel 等代理断开
			if !cw.Fprintf("event: ping\ndata: {\"type\": \"ping\"}\n\n") {
				logger.LegacyPrintf("service.antigravity_gateway", "Client disconnected during keepalive ping (antigravity upstream), continuing to drain upstream for billing")
				continue
			}
		}
	}
}

// extractSSEUsage 从 SSE data 行中提取 Claude usage（用于流式透传场景）
//
// Anthropic streaming 的 usage 字段分布在两类事件中：
//   - message_start：嵌套在 event.message.usage（input_tokens、cache_creation_input_tokens、
//     cache_read_input_tokens 等输入侧字段）
//   - message_delta：位于顶层 event.usage（流结束时的最终 output_tokens）
//
// 仅读取顶层 event.usage 会漏掉 message_start 的输入侧字段，导致流式透传请求落库的
// usage_logs 记录 input_tokens=0。
func (s *AntigravityGatewayService) extractSSEUsage(line string, usage *ClaudeUsage) {
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	dataStr := strings.TrimPrefix(line, "data: ")
	var event map[string]any
	if json.Unmarshal([]byte(dataStr), &event) != nil {
		return
	}
	var u map[string]any
	if eventType, _ := event["type"].(string); eventType == "message_start" {
		if msg, ok := event["message"].(map[string]any); ok {
			u, _ = msg["usage"].(map[string]any)
		}
	} else {
		u, _ = event["usage"].(map[string]any)
	}
	if u == nil {
		return
	}
	if v, ok := u["input_tokens"].(float64); ok && int(v) > 0 {
		usage.InputTokens = int(v)
	}
	if v, ok := u["output_tokens"].(float64); ok && int(v) > 0 {
		usage.OutputTokens = int(v)
	}
	if v, ok := u["cache_read_input_tokens"].(float64); ok && int(v) > 0 {
		usage.CacheReadInputTokens = int(v)
	}
	if v, ok := u["cache_creation_input_tokens"].(float64); ok && int(v) > 0 {
		usage.CacheCreationInputTokens = int(v)
	}
	// 解析嵌套的 cache_creation 对象中的 5m/1h 明细
	if cc, ok := u["cache_creation"].(map[string]any); ok {
		if v, ok := cc["ephemeral_5m_input_tokens"].(float64); ok {
			usage.CacheCreation5mTokens = int(v)
		}
		if v, ok := cc["ephemeral_1h_input_tokens"].(float64); ok {
			usage.CacheCreation1hTokens = int(v)
		}
	}
}

// extractClaudeUsage 从非流式 Claude 响应提取 usage
func (s *AntigravityGatewayService) extractClaudeUsage(body []byte) *ClaudeUsage {
	usage := &ClaudeUsage{}
	var resp map[string]any
	if json.Unmarshal(body, &resp) != nil {
		return usage
	}
	if u, ok := resp["usage"].(map[string]any); ok {
		if v, ok := u["input_tokens"].(float64); ok {
			usage.InputTokens = int(v)
		}
		if v, ok := u["output_tokens"].(float64); ok {
			usage.OutputTokens = int(v)
		}
		if v, ok := u["cache_read_input_tokens"].(float64); ok {
			usage.CacheReadInputTokens = int(v)
		}
		if v, ok := u["cache_creation_input_tokens"].(float64); ok {
			usage.CacheCreationInputTokens = int(v)
		}
		// 解析嵌套的 cache_creation 对象中的 5m/1h 明细
		if cc, ok := u["cache_creation"].(map[string]any); ok {
			if v, ok := cc["ephemeral_5m_input_tokens"].(float64); ok {
				usage.CacheCreation5mTokens = int(v)
			}
			if v, ok := cc["ephemeral_1h_input_tokens"].(float64); ok {
				usage.CacheCreation1hTokens = int(v)
			}
		}
	}
	return usage
}
