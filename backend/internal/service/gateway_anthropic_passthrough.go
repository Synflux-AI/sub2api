package service

// Anthropic APIKey 直通（passthrough）转发路径，以及流式/非流式响应和 usage 解析。

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

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

type anthropicPassthroughForwardInput struct {
	Body          []byte
	Parsed        *ParsedRequest
	RequestModel  string
	OriginalModel string
	RequestStream bool
	StartTime     time.Time
}

func (s *GatewayService) forwardAnthropicAPIKeyPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	reqModel string,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	return s.forwardAnthropicAPIKeyPassthroughWithInput(ctx, c, account, anthropicPassthroughForwardInput{
		Body:          body,
		RequestModel:  reqModel,
		OriginalModel: originalModel,
		RequestStream: reqStream,
		StartTime:     startTime,
	})
}

func (s *GatewayService) forwardAnthropicAPIKeyPassthroughWithInput(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	input anthropicPassthroughForwardInput,
) (*ForwardResult, error) {
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if tokenType != "apikey" {
		return nil, fmt.Errorf("anthropic api key passthrough requires apikey token, got: %s", tokenType)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	logger.CtxPrintf(ctx, "service.gateway", "[Anthropic 自动透传] 命中 API Key 透传分支: account=%d name=%s model=%s stream=%v",
		account.ID, account.Name, input.RequestModel, input.RequestStream)

	if c != nil {
		c.Set("anthropic_passthrough", true)
	}
	// Pre-filter: strip empty text blocks (including nested in tool_result) to prevent upstream 400.
	input.Body = StripEmptyTextBlocks(input.Body)
	// Pre-filter: strip web-search history blocks the upstream cannot accept
	// (emulation-synthesized ones always; genuine ones additionally for
	// passback-required third-party upstreams such as GLM/Kimi/DeepSeek,
	// which reject server_tool_use with 400). input.RequestModel 已是映射后的模型 ID。
	input.Body = FilterWebSearchHistoryBlocks(input.Body, input.RequestModel)
	if input.Parsed != nil {
		// 透传分支也会改写实际 wire body，成功 usage hash 依赖这里同步当前 body。
		if err := input.Parsed.ReplaceBody(input.Body); err != nil {
			return nil, err
		}
	}

	var resp *http.Response
	retryStart := time.Now()
	var errorRuleTracker errorHandlingRuleTracker
	var streamState anthropicPassthroughStreamState
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, input.RequestStream)
		upstreamReq, wireBody, err := s.buildUpstreamRequestAnthropicAPIKeyPassthrough(upstreamCtx, c, account, input.Body, token)
		releaseUpstreamCtx()
		if err != nil {
			return nil, err
		}
		if input.Parsed != nil && !bytes.Equal(wireBody, input.Body) {
			// build 阶段会按 beta 能力清理 body，发送前同步到 ParsedRequest 当前视图。
			if err := input.Parsed.ReplaceBody(wireBody); err != nil {
				return nil, err
			}
			input.Body = input.Parsed.Body.Bytes()
		}

		account.ApplyCustomHeaders(upstreamReq)
		resp, err = s.timedGatewayUpstreamDoWithTLS(c, upstreamReq, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, context.Canceled) {
				scheduleOllamaCloudUsageActivity(s.deferredService, account)
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			setOpsUpstreamError(c, 0, safeErr, "")
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: 0,
				UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
				Passthrough:        true,
				Kind:               "request_error",
				Message:            safeErr,
			})
			c.JSON(http.StatusBadGateway, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "upstream_error",
					"message": "Upstream request failed",
				},
			})
			return nil, fmt.Errorf("upstream request failed: %s", safeErr)
		}

		// Rules take priority over the passthrough path's existing status table.
		// Retry resends input.Body unchanged; activation is checked before reading the body.
		if resp.StatusCode >= 400 && s.errorHandlingRulesActive(ctx, account) {
			ruleRespBody, readErr := s.readUpstreamErrorBody(resp)
			if readErr == nil {
				_ = resp.Body.Close()
				outcome, result, ruleErr := s.applyErrorHandlingRule(ctx, c, &errorRuleTracker, account, resp, ruleRespBody, input.RequestModel, attempt, retryStart, true)
				switch outcome {
				case errorHandlingRuleOutcomeDone:
					return result, ruleErr
				case errorHandlingRuleOutcomeRetry:
					continue
				}
				resp.Body = io.NopCloser(bytes.NewReader(ruleRespBody))
			} else {
				// Preserve the partial body for the existing fallback when upstream reading fails.
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(ruleRespBody))
			}
		}

		// 透传分支禁止 400 请求体降级重试（该重试会改写请求体）
		if resp.StatusCode >= 400 && resp.StatusCode != 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
			if attempt < maxRetryAttempts {
				elapsed := time.Since(retryStart)
				if elapsed >= maxRetryElapsed {
					break
				}

				delay := retryBackoffDelay(attempt)
				remaining := maxRetryElapsed - elapsed
				if delay > remaining {
					delay = remaining
				}
				if delay <= 0 {
					break
				}

				respBody, _ := s.readUpstreamErrorBody(resp)
				_ = resp.Body.Close()
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
					Passthrough:        true,
					Kind:               "retry",
					Message:            extractUpstreamErrorMessage(respBody),
					Detail: func() string {
						if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
							return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
						}
						return ""
					}(),
				})
				logger.CtxPrintf(ctx, "service.gateway", "Anthropic passthrough account %d: upstream error %d, retry %d/%d after %v (elapsed=%v/%v)",
					account.ID, resp.StatusCode, attempt, maxRetryAttempts, delay, elapsed, maxRetryElapsed)
				if err := sleepWithContext(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			break
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && input.RequestStream {
			streamResult, ruleMatch, streamErr := s.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
				ctx, resp, c, account, input.StartTime, input.RequestModel, &errorRuleTracker, &streamState, attempt,
			)
			_ = resp.Body.Close()
			if ruleMatch != nil {
				s.logErrorHandlingRuleDecision(ctx, c, account, ruleMatch.statusCode, resp.Header, ruleMatch.body, attempt, true, ruleMatch.decision)
				switch ruleMatch.decision.EffectiveAction {
				case ErrorHandlingActionRetry:
					if ruleMatch.decision.RetryDelay > 0 {
						if err := sleepWithContext(ctx, ruleMatch.decision.RetryDelay); err != nil {
							return nil, err
						}
					}
					continue
				case ErrorHandlingActionFailover:
					virtualResp := &http.Response{StatusCode: ruleMatch.statusCode, Header: resp.Header.Clone(), Body: http.NoBody}
					err := s.errorHandlingRuleFailover(ctx, virtualResp, ruleMatch.body, account, input.RequestModel, ruleMatch.decision, true)
					if failoverErr, ok := err.(*UpstreamFailoverError); ok {
						failoverErr.SafeToFailoverAfterWrite = !ruleMatch.semanticEventForwarded
					}
					return nil, err
				case ErrorHandlingActionPassthrough:
					err := s.writeAnthropicPassthroughStreamRuleError(ctx, c, resp, account, input.RequestModel, ruleMatch)
					if partial := partialStreamUsageResult(c, resp, streamResult, input.OriginalModel, input.RequestModel, input.StartTime, err); partial != nil {
						return partial, err
					}
					return nil, err
				}
			}
			if streamErr != nil {
				if partial := partialStreamUsageResult(c, resp, streamResult, input.OriginalModel, input.RequestModel, input.StartTime, streamErr); partial != nil {
					return partial, streamErr
				}
				return nil, streamErr
			}
			if streamResult == nil || streamResult.usage == nil {
				streamResult = &streamingResult{usage: &ClaudeUsage{}}
			}
			return &ForwardResult{
				RequestID:                     resp.Header.Get("x-request-id"),
				Usage:                         *streamResult.usage,
				Model:                         input.OriginalModel,
				UpstreamModel:                 input.RequestModel,
				UpstreamResponseModel:         observedUpstreamResponseModel(c),
				UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
				Stream:                        true,
				Duration:                      time.Since(input.StartTime),
				FirstTokenMs:                  streamResult.firstTokenMs,
				ClientDisconnect:              streamResult.clientDisconnect,
			}, nil
		}

		break
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("upstream request failed: empty response")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			respBody, _ := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			logger.CtxPrintf(ctx, "service.gateway", "[Anthropic Passthrough] Upstream error (retry exhausted, failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
				account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

			s.handleRetryExhaustedSideEffects(ctx, resp, account)
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Passthrough:        true,
				Kind:               "retry_exhausted_failover",
				Message:            extractUpstreamErrorMessage(respBody),
				Detail: func() string {
					if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
						return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
					}
					return ""
				}(),
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleRetryExhaustedError(ctx, resp, c, account)
	}

	if resp.StatusCode >= 400 && s.shouldFailoverUpstreamError(resp.StatusCode) {
		respBody, _ := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		logger.CtxPrintf(ctx, "service.gateway", "[Anthropic Passthrough] Upstream error (failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
			account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

		s.handleFailoverSideEffects(ctx, resp, account, input.RequestModel)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Passthrough:        true,
			Kind:               "failover",
			Message:            extractUpstreamErrorMessage(respBody),
			Detail: func() string {
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
				}
				return ""
			}(),
		})
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           respBody,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	// 签名错误切换账号（直传路径）：直传禁止去签名重试（会改写请求体），
	// 但切换账号不改写 body，故签名相关 400/422 直接触发 failover 换号。
	// 复用 shouldFailoverSignatureError，受同一个「API Key 签名错误切换账号」开关控制。
	if isSignatureFailoverStatus(resp.StatusCode) {
		respBody, readErr := s.readUpstreamErrorBody(resp)
		if readErr == nil {
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			if s.shouldFailoverSignatureError(ctx, account, respBody, input.RequestModel) {
				upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Passthrough:        true,
					Kind:               "signature_failover",
					Message:            upstreamMsg,
					Detail: func() string {
						if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
							return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
						}
						return ""
					}(),
				})
				logger.CtxPrintf(ctx, "service.gateway", "[Anthropic Passthrough] Account %d: signature error, switching account", account.ID)
				s.handleFailoverSideEffects(ctx, resp, account, input.RequestModel)
				return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBody}
			}
		}
	}

	if resp.StatusCode >= 400 {
		return s.handleErrorResponse(ctx, resp, c, account, input.RequestModel)
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	var clientDisconnect bool
	if !input.RequestStream {
		usage, err = s.handleNonStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account)
		if err != nil {
			return nil, err
		}
	}
	if usage == nil {
		usage = &ClaudeUsage{}
	}

	return &ForwardResult{
		RequestID:                     resp.Header.Get("x-request-id"),
		Usage:                         *usage,
		Model:                         input.OriginalModel,
		UpstreamModel:                 input.RequestModel,
		UpstreamResponseModel:         observedUpstreamResponseModel(c),
		UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
		Stream:                        input.RequestStream,
		Duration:                      time.Since(input.StartTime),
		FirstTokenMs:                  firstTokenMs,
		ClientDisconnect:              clientDisconnect,
	}, nil
}

func (s *GatewayService) buildUpstreamRequestAnthropicAPIKeyPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) (*http.Request, []byte, error) {
	body = stripDeferredToolCacheControl(body)
	targetURL := claudeAPIURL
	baseURL := account.GetBaseURL()
	if baseURL != "" {
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return nil, nil, err
		}
		targetURL = validatedURL + "/v1/messages?beta=true"
	}

	// 能力维度 body sanitize：透传路径上 anthropic-beta header 原样透传客户端值，
	// 依此决定是否保留 body 中的 context_management。避免“客户端 body 带字段但
	// header 忘记带 beta token”的客户端 bug 在透传场景下让上游 400。
	clientBeta := ""
	if c != nil && c.Request != nil {
		clientBeta = getHeaderRaw(c.Request.Header, "anthropic-beta")
	}
	// 账号覆写了 anthropic-beta 时，覆写值即最终上游值：净化以覆写值为准
	if beta, ok := account.HeaderOverrideValue("anthropic-beta"); ok {
		clientBeta = beta
	}
	if sanitized, changed := sanitizeAnthropicBodyForBetaTokens(body, clientBeta); changed {
		body = sanitized
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if !allowedHeaders[lowerKey] {
				continue
			}
			wireKey := resolveWireCasing(key)
			for _, v := range values {
				addHeaderRaw(req.Header, wireKey, v)
			}
		}
	}

	// 链路关联 ID（账号开启 trace_id_passthrough 时才注入）
	injectTraceHeader(ctx, req, account)

	// 覆盖入站鉴权残留，并注入上游认证
	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	req.Header.Del("cookie")
	setAnthropicAPIKeyAuthHeader(req.Header, account, token)

	if getHeaderRaw(req.Header, "content-type") == "" {
		setHeaderRaw(req.Header, "content-type", "application/json")
	}
	if getHeaderRaw(req.Header, "anthropic-version") == "" {
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	}

	// 账号级请求头覆写（最终生效，覆盖上面所有来源的同名头）
	account.ApplyHeaderOverrides(req.Header)

	return req, body, nil
}

const anthropicPassthroughSSEFramingAllowance = 4 * 1024

type anthropicPassthroughSSEEvent struct {
	raw       []byte
	eventName string
	data      []byte
	// hasDataLine 记录本事件里是否真有 `data:` 行。data 可能来自畸形裸 JSON 错误体的
	// 补齐（见 bareAnthropicPassthroughErrorPayload），那种补齐不算「向客户端发过输出」，
	// 所以语义判定只认这个标志，不认 len(data)（#190）。
	hasDataLine bool
}

type anthropicPassthroughStreamRuleMatch struct {
	decision               errorHandlingRuleDecision
	statusCode             int
	body                   []byte
	errType                string
	errMessage             string
	rawEvent               []byte
	semanticEventForwarded bool
	synthetic              bool
}

type anthropicPassthroughStreamState struct {
	semanticEventForwarded bool
}

func buildAnthropicPassthroughSSEEvent(lines []string, terminated bool) anthropicPassthroughSSEEvent {
	var raw strings.Builder
	dataLines := make([]string, 0, 1)
	eventName := ""
	for _, line := range lines {
		_, _ = raw.WriteString(line)
		_ = raw.WriteByte('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		}
		if data, ok := extractAnthropicSSEDataLine(line); ok {
			dataLines = append(dataLines, data)
		}
	}
	if terminated {
		_ = raw.WriteByte('\n')
	}
	event := anthropicPassthroughSSEEvent{
		raw:         []byte(raw.String()),
		eventName:   eventName,
		data:        []byte(strings.Join(dataLines, "\n")),
		hasDataLine: len(dataLines) > 0,
	}
	if !event.hasDataLine {
		// 上游偶发在 200 流里直接写一行没有 `data:` 前缀的 JSON 错误体。原样丢弃会让
		// 错误处理规则一级匹配彻底拿不到上游原文（#190），这里只在「零 data 行 + 整块是
		// 单个合法 JSON 对象 + type == error」三重收窄下把它补进 data 供分类使用；
		// 对客转发的 event.raw 不受影响。
		if payload, ok := bareAnthropicPassthroughErrorPayload(event.raw); ok {
			event.data = payload
		}
	}
	return event
}

// bareAnthropicPassthroughErrorPayload 判断整个事件块是否就是一个 Anthropic 错误对象
// 本身（没有任何 SSE 字段前缀）。json.Unmarshal 到 map 同时排掉数组、标量与尾随垃圾。
func bareAnthropicPassthroughErrorPayload(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) != nil {
		return nil, false
	}
	var eventType string
	if err := json.Unmarshal(fields["type"], &eventType); err != nil || eventType != "error" {
		return nil, false
	}
	return trimmed, true
}

func anthropicPassthroughSSEError(event anthropicPassthroughSSEEvent) (int, string, string, bool) {
	var payload anthropicSafeError
	parsed := json.Unmarshal(event.data, &payload) == nil
	errorObjectNonEmpty := false
	if parsed && payload.Type == "error" {
		var envelope map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(event.data, &envelope) == nil && json.Unmarshal(envelope["error"], &fields) == nil {
			errorObjectNonEmpty = len(fields) > 0
		}
	}
	isError := strings.EqualFold(strings.TrimSpace(event.eventName), "error") ||
		(parsed && payload.Type == "error" && payload.Error != nil && errorObjectNonEmpty)
	if !isError {
		return 0, "", "", false
	}
	errType, message := safeAnthropicError(event.data)
	statusCode := http.StatusInternalServerError
	switch errType {
	case "invalid_request_error":
		statusCode = http.StatusBadRequest
	case "authentication_error":
		statusCode = http.StatusUnauthorized
	case "permission_error":
		statusCode = http.StatusForbidden
	case "not_found_error":
		statusCode = http.StatusNotFound
	case "request_too_large":
		statusCode = http.StatusRequestEntityTooLarge
	case "rate_limit_error":
		statusCode = http.StatusTooManyRequests
	case "overloaded_error":
		statusCode = 529
	}
	return statusCode, errType, message, true
}

func anthropicPassthroughSSEEventIsSemantic(event anthropicPassthroughSSEEvent) bool {
	if _, _, _, isError := anthropicPassthroughSSEError(event); isError {
		return false
	}
	// SSE 里不带 data 字段的事件不承载 payload，不该算「已向客户端发出输出」。真实输出
	// 一律以 `data:` 承载。谓词与 processEvent 里的 TrimSpace(data) != "" 保持一致，
	// 这样 semanticEventForwarded == true ⇒ firstTokenMs != nil 才成立（#190）。
	if !event.hasDataLine || len(bytes.TrimSpace(event.data)) == 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(event.eventName), "ping") {
		return false
	}
	if gjson.GetBytes(event.data, "type").String() == "ping" {
		return false
	}
	// [DONE] 由 anthropicStreamEventIsTerminal 单独判终止，不是语义输出。
	if string(bytes.TrimSpace(event.data)) == "[DONE]" {
		return false
	}
	// 走到这里必然存在一条携带非空 payload 的 `data:` 行（注释行与空事件已被上面的
	// hasDataLine 闸门挡掉），即真实语义输出。
	return true
}

func (s *GatewayService) handleStreamingResponseAnthropicAPIKeyPassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	model string,
) (*streamingResult, error) {
	result, match, err := s.handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
		ctx, resp, c, account, startTime, model, nil, nil, 0,
	)
	if match != nil {
		return result, fmt.Errorf("stream matched error handling rule %s", match.decision.RuleID)
	}
	return result, err
}

func (s *GatewayService) handleStreamingResponseAnthropicAPIKeyPassthroughWithRules(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	model string,
	ruleTracker *errorHandlingRuleTracker,
	streamState *anthropicPassthroughStreamState,
	attempt int,
) (result *streamingResult, ruleMatch *anthropicPassthroughStreamRuleMatch, err error) {
	var unmatchedStreamError *unmatchedStreamErrorEvent
	// hit-priority：只要这次 attempt 最终返回了规则命中，未命中记录一律丢弃。
	// 用 defer 覆盖所有 return 分支，包括后续新增的。`ruleMatch != nil` 是抑制
	// 未命中记录的唯一机制——不依赖 processEvent 内部是否清空过
	// unmatchedStreamError，命中分支到这里只经由具名返回值传播。
	defer func() {
		if ruleMatch != nil {
			return
		}
		s.flushUnmatchedStreamError(ctx, c, account, model, attempt, unmatchedStreamError)
	}()
	if streamState == nil {
		streamState = &anthropicPassthroughStreamState{}
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	// Only transport headers are stable across attempts. Attempt-local headers are
	// applied immediately before the first semantic event, if they are still mutable.
	c.Header("Content-Type", "text/event-stream")
	if c.Writer.Header().Get("Cache-Control") == "" {
		c.Header("Cache-Control", "no-cache")
	}
	if c.Writer.Header().Get("Connection") == "" {
		c.Header("Connection", "keep-alive")
	}
	c.Header("X-Accel-Buffering", "no")
	applyAttemptHeaders := func() {
		if c.Writer.Written() {
			return
		}
		writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, nil, errors.New("streaming not supported")
	}

	usage := &ClaudeUsage{}
	var firstTokenMs *int
	clientDisconnected := false
	sawTerminalEvent := false
	// Keep semantic output sticky across rule retries. This remains a safety net
	// even though a matched retry/failover is already downgraded after semantics.
	semanticEventForwarded := streamState.semanticEventForwarded
	sawAnyErrorEvent := false

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	maxEventSize := maxLineSize + anthropicPassthroughSSEFramingAllowance
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)

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
	go func(scanBuf *sseScannerBuf64K) {
		defer putSSEScannerBuf64K(scanBuf)
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
	}(scanBuf)
	defer close(done)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
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

	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	var keepaliveTimer *time.Timer
	if keepaliveInterval > 0 {
		keepaliveTimer = time.NewTimer(keepaliveInterval)
		defer keepaliveTimer.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTimer != nil {
		keepaliveCh = keepaliveTimer.C
	}
	lastDataAt := time.Now()
	resetKeepaliveTimer := func() {
		if keepaliveTimer == nil {
			return
		}
		if !keepaliveTimer.Stop() {
			select {
			case <-keepaliveTimer.C:
			default:
			}
		}
		keepaliveTimer.Reset(keepaliveInterval)
	}
	pendingEventLines := make([]string, 0, 4)
	pendingEventSize := 0
	resultWithUsage := func() *streamingResult {
		return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnected}
	}
	writeEvent := func(event anthropicPassthroughSSEEvent, semantic bool) {
		if clientDisconnected {
			return
		}
		if semantic {
			applyAttemptHeaders()
		}
		restored := reverseToolNamesIfPresent(c, event.raw)
		n, err := w.Write(restored)
		if semantic && n > 0 {
			semanticEventForwarded = true
			streamState.semanticEventForwarded = true
		}
		if err != nil {
			clientDisconnected = true
			logger.CtxPrintf(ctx, "service.gateway", "[Anthropic passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
			return
		}
		flusher.Flush()
		lastDataAt = time.Now()
		resetKeepaliveTimer()
	}
	processEvent := func(event anthropicPassthroughSSEEvent) (*anthropicPassthroughStreamRuleMatch, error) {
		statusCode, errType, errMessage, isError := anthropicPassthroughSSEError(event)
		if isError {
			// Do not synthesize a second EOF error after any upstream error event.
			// Unmatched events intentionally retain the legacy fallback contract;
			// matched events end this attempt immediately with one rule decision.
			sawAnyErrorEvent = true
			// Once the downstream is gone, keep draining only to collect usage. A rule
			// retry/failover would discard that partial result and can create extra
			// upstream spend for a request that no longer has a receiver (#5148).
			if !clientDisconnected && ctx.Err() == nil {
				decision := s.decideErrorHandlingRule(ctx, ruleTracker, account, statusCode, event.data, model, errorHandlingRuleDecisionOptions{
					Attempt: attempt, IgnoreRetryElapsed: true, SemanticEventForwarded: semanticEventForwarded, IndependentRetryBudget: true,
				})
				if decision.Matched {
					return &anthropicPassthroughStreamRuleMatch{
						decision: decision, statusCode: statusCode, body: append([]byte(nil), event.data...),
						errType: errType, errMessage: errMessage, rawEvent: append([]byte(nil), event.raw...),
						semanticEventForwarded: semanticEventForwarded,
					}, nil
				}
				if unmatchedStreamError == nil {
					unmatchedStreamError = &unmatchedStreamErrorEvent{
						statusCode: statusCode,
						errType:    errType,
						message:    errMessage,
						detail:     string(event.data),
					}
				}
			}
		}

		semantic := anthropicPassthroughSSEEventIsSemantic(event)
		data := strings.TrimSpace(string(event.data))
		if data != "" {
			observer.ObserveAnthropic([]byte(data))
			if anthropicStreamEventIsTerminal(event.eventName, data) {
				sawTerminalEvent = true
			}
			if semantic && firstTokenMs == nil && data != "[DONE]" {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			parseSSEUsagePassthrough(data, usage)
		} else if anthropicStreamEventIsTerminal(event.eventName, "") {
			sawTerminalEvent = true
		}
		writeEvent(event, semantic)
		return nil, nil
	}
	processPendingEvent := func(terminated bool) (*anthropicPassthroughStreamRuleMatch, error) {
		if len(pendingEventLines) == 0 {
			if terminated {
				writeEvent(anthropicPassthroughSSEEvent{raw: []byte("\n")}, false)
			}
			return nil, nil
		}
		event := buildAnthropicPassthroughSSEEvent(pendingEventLines, terminated)
		pendingEventLines = pendingEventLines[:0]
		pendingEventSize = 0
		return processEvent(event)
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if match, err := processPendingEvent(false); match != nil || err != nil {
					return resultWithUsage(), match, err
				}
				if !sawTerminalEvent {
					if clientDisconnected && streamInterval > 0 {
						lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
						if time.Since(lastRead) >= streamInterval {
							return resultWithUsage(), nil, fmt.Errorf("stream usage incomplete after timeout")
						}
					}
					if !semanticEventForwarded && !sawAnyErrorEvent && !clientDisconnected && ctx.Err() == nil {
						body := []byte(`{"type":"error","error":{"type":"stream_error","message":"stream usage incomplete: missing terminal event"}}`)
						decision := s.decideErrorHandlingRule(ctx, ruleTracker, account, http.StatusBadGateway, body, model, errorHandlingRuleDecisionOptions{
							Attempt: attempt, IgnoreRetryElapsed: true, IndependentRetryBudget: true,
						})
						if decision.Matched {
							return resultWithUsage(), &anthropicPassthroughStreamRuleMatch{
								decision: decision, statusCode: http.StatusBadGateway, body: body,
								errType: "stream_error", errMessage: "stream usage incomplete: missing terminal event",
								synthetic: true,
							}, nil
						}
					}
					s.recordStreamFailureCause(ctx, c, account, model,
						streamFailureMissingTerminal, "stream usage incomplete: missing terminal event", firstTokenMs != nil, clientDisconnected)
					return resultWithUsage(), nil, fmt.Errorf("stream usage incomplete: missing terminal event")
				}
				return resultWithUsage(), nil, nil
			}
			if ev.err != nil {
				if sawTerminalEvent {
					return resultWithUsage(), nil, nil
				}
				if clientDisconnected {
					return resultWithUsage(), nil, fmt.Errorf("stream usage incomplete after disconnect: %w", ev.err)
				}
				if errors.Is(ev.err, context.Canceled) || errors.Is(ev.err, context.DeadlineExceeded) {
					clientDisconnected = true
					return resultWithUsage(), nil, fmt.Errorf("stream usage incomplete: %w", ev.err)
				}
				if errors.Is(ev.err, bufio.ErrTooLong) {
					logger.CtxPrintf(ctx, "service.gateway", "[Anthropic passthrough] SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, ev.err)
					return resultWithUsage(), nil, ev.err
				}
				s.recordStreamFailureCause(ctx, c, account, model,
					streamFailureReadError, fmt.Sprintf("stream read error: %v", ev.err), firstTokenMs != nil, clientDisconnected)
				return resultWithUsage(), nil, fmt.Errorf("stream read error: %w", ev.err)
			}

			line := ev.line
			if line == "" {
				if match, err := processPendingEvent(true); match != nil || err != nil {
					return resultWithUsage(), match, err
				}
				continue
			}
			pendingEventSize += len(line) + 1
			if pendingEventSize > maxEventSize {
				logger.CtxPrintf(ctx, "service.gateway", "[Anthropic passthrough] SSE event too long: account=%d max_size=%d", account.ID, maxEventSize)
				return resultWithUsage(), nil, bufio.ErrTooLong
			}
			pendingEventLines = append(pendingEventLines, line)

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if clientDisconnected {
				return resultWithUsage(), nil, fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.CtxPrintf(ctx, "service.gateway", "[Anthropic passthrough] Stream data interval timeout: account=%d model=%s interval=%s", account.ID, model, streamInterval)
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, model)
			}
			s.recordStreamFailureCause(ctx, c, account, model,
				streamFailureIntervalTimeout, "stream data interval timeout", firstTokenMs != nil, clientDisconnected)
			return resultWithUsage(), nil, fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if clientDisconnected {
				continue
			}
			if len(pendingEventLines) > 0 {
				resetKeepaliveTimer()
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				resetKeepaliveTimer()
				continue
			}
			if _, err := fmt.Fprint(w, "event: ping\ndata: {\"type\": \"ping\"}\n\n"); err != nil {
				clientDisconnected = true
				logger.CtxPrintf(ctx, "service.gateway", "[Anthropic passthrough] Client disconnected during keepalive ping, continue draining upstream for usage: account=%d", account.ID)
				continue
			}
			flusher.Flush()
			lastDataAt = time.Now()
			resetKeepaliveTimer()
		}
	}
}

func (s *GatewayService) writeAnthropicPassthroughStreamRuleError(
	ctx context.Context,
	c *gin.Context,
	resp *http.Response,
	account *Account,
	model string,
	match *anthropicPassthroughStreamRuleMatch,
) error {
	if match == nil {
		return errors.New("missing Anthropic stream rule match")
	}
	scheduleOllamaCloudUsageActivity(s.deferredService, account)
	if s.rateLimitService != nil {
		_ = s.rateLimitService.HandleUpstreamError(ctx, account, match.statusCode, resp.Header, match.body, model)
	}
	setOpsUpstreamError(c, match.statusCode, match.errMessage, "")
	MarkOpsStreamError(c, match.errType, match.errMessage, match.statusCode)

	if !c.Writer.Written() {
		writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
	}
	event := match.rawEvent
	if match.synthetic {
		event = []byte("event: error\ndata: " + string(match.body) + "\n\n")
	}
	if len(event) > 0 {
		if _, err := c.Writer.Write(event); err != nil {
			MarkResponseCommitted(c)
			return err
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	MarkResponseCommitted(c)
	return fmt.Errorf("upstream SSE error: %d (error handling rule passthrough) message=%s", match.statusCode, match.errMessage)
}

func extractAnthropicSSEDataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '\t' {
			break
		}
		start++
	}
	return line[start:], true
}

// parseSSEUsagePassthrough 从 Anthropic SSE data 行提取 usage（包级函数：
// Anthropic 平台 passthrough 与国产供应商原生 Anthropic 直通共用）。
func parseSSEUsagePassthrough(data string, usage *ClaudeUsage) {
	if usage == nil || data == "" || data == "[DONE]" {
		return
	}

	parsed := gjson.Parse(data)
	switch parsed.Get("type").String() {
	case "message_start":
		msgUsage := parsed.Get("message.usage")
		if msgUsage.Exists() {
			usage.InputTokens = int(msgUsage.Get("input_tokens").Int())
			usage.CacheCreationInputTokens = int(msgUsage.Get("cache_creation_input_tokens").Int())
			usage.CacheReadInputTokens = int(msgUsage.Get("cache_read_input_tokens").Int())

			// 保持与通用解析一致：message_start 允许覆盖 5m/1h 明细（包括 0）。
			cc5m := msgUsage.Get("cache_creation.ephemeral_5m_input_tokens")
			cc1h := msgUsage.Get("cache_creation.ephemeral_1h_input_tokens")
			if cc5m.Exists() || cc1h.Exists() {
				usage.CacheCreation5mTokens = int(cc5m.Int())
				usage.CacheCreation1hTokens = int(cc1h.Int())
			}
		}
	case "message_delta":
		deltaUsage := parsed.Get("usage")
		if deltaUsage.Exists() {
			if v := deltaUsage.Get("input_tokens").Int(); v > 0 {
				usage.InputTokens = int(v)
			}
			if v := deltaUsage.Get("output_tokens").Int(); v > 0 {
				usage.OutputTokens = int(v)
			}
			if v := deltaUsage.Get("cache_creation_input_tokens").Int(); v > 0 {
				usage.CacheCreationInputTokens = int(v)
			}
			if v := deltaUsage.Get("cache_read_input_tokens").Int(); v > 0 {
				usage.CacheReadInputTokens = int(v)
			}

			cc5m := deltaUsage.Get("cache_creation.ephemeral_5m_input_tokens")
			cc1h := deltaUsage.Get("cache_creation.ephemeral_1h_input_tokens")
			if cc5m.Exists() {
				usage.CacheCreation5mTokens = int(cc5m.Int())
			}
			if cc1h.Exists() {
				usage.CacheCreation1hTokens = int(cc1h.Int())
			}
		}
	}

	if usage.CacheReadInputTokens == 0 {
		if cached := parsed.Get("message.usage.cached_tokens").Int(); cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
		if cached := parsed.Get("usage.cached_tokens").Int(); usage.CacheReadInputTokens == 0 && cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
	}
	if usage.CacheCreationInputTokens == 0 {
		cc5m := parsed.Get("message.usage.cache_creation.ephemeral_5m_input_tokens").Int()
		cc1h := parsed.Get("message.usage.cache_creation.ephemeral_1h_input_tokens").Int()
		if cc5m == 0 && cc1h == 0 {
			cc5m = parsed.Get("usage.cache_creation.ephemeral_5m_input_tokens").Int()
			cc1h = parsed.Get("usage.cache_creation.ephemeral_1h_input_tokens").Int()
		}
		total := cc5m + cc1h
		if total > 0 {
			usage.CacheCreationInputTokens = int(total)
		}
	}

	// Kimi's Anthropic-compatible stream uses input_tokens with two meanings:
	// message_start reports total prompt input, while message_delta reports only
	// uncached input. prompt_tokens remains the total in both events. Normalize
	// to ClaudeUsage's mutually-exclusive buckets so downstream billing does not
	// subtract cache tokens from an already-uncached value.
	usageNode := parsed.Get("usage")
	if parsed.Get("type").String() == "message_start" {
		usageNode = parsed.Get("message.usage")
	}
	normalizeAnthropicCompatiblePromptUsage(usageNode, usage)
}

// normalizeAnthropicCompatiblePromptUsage converts provider-native OpenAI-style
// prompt/cache fields into Claude's mutually-exclusive usage buckets. Native
// Anthropic responses do not expose these aliases and are left alone.
func normalizeAnthropicCompatiblePromptUsage(usageNode gjson.Result, usage *ClaudeUsage) bool {
	if usage == nil || !usageNode.Exists() {
		return false
	}
	promptTokens := usageNode.Get("prompt_tokens")
	promptCacheHitTokens := usageNode.Get("prompt_cache_hit_tokens")
	promptCacheMissTokens := usageNode.Get("prompt_cache_miss_tokens")
	if (!promptTokens.Exists() || promptTokens.Int() <= 0) &&
		!promptCacheHitTokens.Exists() && !promptCacheMissTokens.Exists() {
		return false
	}

	cacheReadTokens := usage.CacheReadInputTokens
	if v := usageNode.Get("cache_read_input_tokens"); v.Exists() {
		cacheReadTokens = int(v.Int())
	}
	if cacheReadTokens == 0 {
		if v := usageNode.Get("cached_tokens"); v.Exists() {
			cacheReadTokens = int(v.Int())
		}
	}
	if cacheReadTokens == 0 {
		if v := usageNode.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			cacheReadTokens = int(v.Int())
		}
	}
	if cacheReadTokens == 0 && promptCacheHitTokens.Exists() {
		cacheReadTokens = max(int(promptCacheHitTokens.Int()), 0)
	}

	cacheCreationTokens := usage.CacheCreationInputTokens
	if v := usageNode.Get("cache_creation_input_tokens"); v.Exists() {
		cacheCreationTokens = int(v.Int())
	}
	if cacheCreationTokens == 0 {
		cc5m := usageNode.Get("cache_creation.ephemeral_5m_input_tokens").Int()
		cc1h := usageNode.Get("cache_creation.ephemeral_1h_input_tokens").Int()
		if cc5m > 0 || cc1h > 0 {
			cacheCreationTokens = int(cc5m + cc1h)
		}
	}

	if promptCacheMissTokens.Exists() {
		usage.InputTokens = max(int(promptCacheMissTokens.Int()), 0)
	} else {
		usage.InputTokens = max(int(promptTokens.Int())-cacheReadTokens-cacheCreationTokens, 0)
	}
	usage.CacheReadInputTokens = cacheReadTokens
	usage.CacheCreationInputTokens = cacheCreationTokens
	return true
}

func parseClaudeUsageFromResponseBody(body []byte) *ClaudeUsage {
	usage := &ClaudeUsage{}
	if len(body) == 0 {
		return usage
	}

	parsed := gjson.ParseBytes(body)
	usageNode := parsed.Get("usage")
	if !usageNode.Exists() {
		return usage
	}

	usage.InputTokens = int(usageNode.Get("input_tokens").Int())
	usage.OutputTokens = int(usageNode.Get("output_tokens").Int())
	usage.CacheCreationInputTokens = int(usageNode.Get("cache_creation_input_tokens").Int())
	usage.CacheReadInputTokens = int(usageNode.Get("cache_read_input_tokens").Int())

	cc5m := usageNode.Get("cache_creation.ephemeral_5m_input_tokens").Int()
	cc1h := usageNode.Get("cache_creation.ephemeral_1h_input_tokens").Int()
	if cc5m > 0 || cc1h > 0 {
		usage.CacheCreation5mTokens = int(cc5m)
		usage.CacheCreation1hTokens = int(cc1h)
	}
	if usage.CacheCreationInputTokens == 0 && (cc5m > 0 || cc1h > 0) {
		usage.CacheCreationInputTokens = int(cc5m + cc1h)
	}
	if usage.CacheReadInputTokens == 0 {
		if cached := usageNode.Get("cached_tokens").Int(); cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
	}
	normalizeAnthropicCompatiblePromptUsage(usageNode, usage)
	return usage
}

// invalidNonStreamingJSONFailoverError 把"上游 2xx 返回非 JSON body"归一为
// failover 错误（包级函数：Anthropic 平台 passthrough 与国产供应商原生
// Anthropic 直通共用）。
func invalidNonStreamingJSONFailoverError(
	ctx context.Context,
	rateLimitService *RateLimitService,
	resp *http.Response,
	account *Account,
	body []byte,
	parseErr error,
	requestedModel ...string,
) error {
	const statusCode = http.StatusBadGateway

	accountID := int64(0)
	accountName := ""
	retryableOnSameAccount := false
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		retryableOnSameAccount = account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
	}

	logger.CtxPrintf(
		ctx,
		"service.gateway",
		"Account %d(%s): upstream returned non-JSON 2xx response, attempting failover: status=%d request_id=%s error=%v",
		accountID,
		accountName,
		resp.StatusCode,
		resp.Header.Get("x-request-id"),
		parseErr,
	)

	if rateLimitService != nil && account != nil {
		if len(requestedModel) > 0 {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body, requestedModel[0])
		} else {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body)
		}
	}

	return &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           body,
		ResponseHeaders:        resp.Header,
		RetryableOnSameAccount: retryableOnSameAccount,
	}
}

func (s *GatewayService) handleNonStreamingResponseAnthropicAPIKeyPassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*ClaudeUsage, error) {
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, anthropicTooLargeError)
	if err != nil {
		return nil, err
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.ObserveAnthropic(body)

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		var raw json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, invalidNonStreamingJSONFailoverError(ctx, s.rateLimitService, resp, account, body, err)
		}
	}

	usage := parseClaudeUsageFromResponseBody(body)
	if IsForceCacheBilling(ctx) && usage.InputTokens > 0 {
		body, err = classifyAnthropicResponseInputAsCacheRead(body, usage)
		if err != nil {
			return nil, err
		}
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	body = reverseToolNamesIfPresent(c, body)
	c.Data(resp.StatusCode, contentType, body)
	return usage, nil
}

func classifyAnthropicResponseInputAsCacheRead(body []byte, usage *ClaudeUsage) ([]byte, error) {
	classified, err := sjson.SetBytes(body, "usage.input_tokens", 0)
	if err != nil {
		return nil, fmt.Errorf("classify forced cache billing input tokens: %w", err)
	}
	classified, err = sjson.SetBytes(classified, "usage.cache_read_input_tokens", usage.CacheReadInputTokens+usage.InputTokens)
	if err != nil {
		return nil, fmt.Errorf("classify forced cache billing cache read tokens: %w", err)
	}
	return classified, nil
}

func writeAnthropicPassthroughResponseHeaders(dst http.Header, src http.Header, filter *responseheaders.CompiledHeaderFilter) {
	if dst == nil || src == nil {
		return
	}
	if filter != nil {
		responseheaders.WriteFilteredHeaders(dst, src, filter)
		return
	}
	if v := strings.TrimSpace(src.Get("Content-Type")); v != "" {
		dst.Set("Content-Type", v)
	}
	if v := strings.TrimSpace(src.Get("x-request-id")); v != "" {
		dst.Set("x-request-id", v)
	}
}
