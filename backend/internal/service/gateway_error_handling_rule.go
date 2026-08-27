package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type anthropicSafeError struct {
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func safeAnthropicError(body []byte) (string, string) {
	var payload anthropicSafeError
	if json.Unmarshal(body, &payload) == nil && payload.Error != nil {
		errType := strings.TrimSpace(payload.Error.Type)
		message := sanitizeUpstreamErrorMessage(strings.TrimSpace(payload.Error.Message))
		if errType != "" && message != "" {
			return errType, message
		}
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if message == "" {
		message = "Upstream request failed"
	}
	return "upstream_error", message
}

func isErrorHandlingRuleAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformAnthropic && account.Type == AccountTypeAPIKey
}

func (s *GatewayService) errorHandlingRulesActive(ctx context.Context, account *Account) bool {
	if !isErrorHandlingRuleAccount(account) || s == nil || s.settingService == nil {
		return false
	}
	settings := s.settingService.GetErrorHandlingRuleSettingsCached(ctx)
	return settings.Enabled && HasEnabledErrorHandlingRule(settings.Rules)
}

func (s *GatewayService) matchErrorHandlingRuleForAccount(ctx context.Context, account *Account, statusCode int, respBody []byte) (*ErrorHandlingRule, int) {
	if !isErrorHandlingRuleAccount(account) || s == nil || s.settingService == nil {
		return nil, 0
	}
	settings := s.settingService.GetErrorHandlingRuleSettingsCached(ctx)
	if !settings.Enabled {
		return nil, 0
	}
	rule := matchErrorHandlingRule(settings.Rules, statusCode, respBody)
	if rule == nil {
		return nil, 0
	}
	return rule, rule.RetryLimit(settings.DefaultRetryCount)
}

// builtinSignatureHandlingOwns 判断这条响应是否归内置的 Thinking 签名逻辑处理。
//
// issue #122 决策 5：内置签名整流/换号保持原样、优先于新引擎。规则如果宽泛匹配
// 400/422，会把签名错误从内置换号手里抢走——签名错误在同一个账号上原样重发必然
// 再次失败，配成 passthrough 更是直接把它吐给客户端。
// 只对 400/422 做这个判断，其余状态码不会付出读配置的代价。
func (s *GatewayService) builtinSignatureHandlingOwns(ctx context.Context, account *Account, statusCode int, respBody []byte, reqModel string) bool {
	return isSignatureFailoverStatus(statusCode) && s.shouldFailoverSignatureError(ctx, account, respBody, reqModel)
}

func restoreErrorHandlingRuleBody(resp *http.Response, respBody []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
}

// decideErrorHandlingRule 是 Anthropic 侧对平台中立决策层的薄包装：只负责
// 「这条错误归不归内置签名逻辑管」「这个账号适不适用规则」两个平台特有判断，
// 其余全部交给 decideErrorHandlingRuleFrom（见 error_handling_rule_decider.go）。
//
// 两个前置判断的先后顺序不能调换：builtinSignatureHandlingOwns 先于账号 gate，
// 与拆分前逐条一致。
func (s *GatewayService) decideErrorHandlingRule(
	ctx context.Context,
	tracker *errorHandlingRuleTracker,
	account *Account,
	statusCode int,
	respBody []byte,
	reqModel string,
	opts errorHandlingRuleDecisionOptions,
) errorHandlingRuleDecision {
	if s.builtinSignatureHandlingOwns(ctx, account, statusCode, respBody, reqModel) {
		return errorHandlingRuleDecision{}
	}
	if !isErrorHandlingRuleAccount(account) || s == nil || s.settingService == nil {
		return errorHandlingRuleDecision{}
	}
	return decideErrorHandlingRuleFrom(errorHandlingRuleDeciderInput{
		Settings:   s.settingService.GetErrorHandlingRuleSettingsCached(ctx),
		Tracker:    tracker,
		StatusCode: statusCode,
		Body:       respBody,
		Opts:       opts,
	})
}

func (s *GatewayService) applyErrorHandlingRule(
	ctx context.Context,
	c *gin.Context,
	tracker *errorHandlingRuleTracker,
	account *Account,
	resp *http.Response,
	respBody []byte,
	reqModel string,
	attempt int,
	retryStart time.Time,
	passthroughPath bool,
) (errorHandlingRuleOutcome, *ForwardResult, error) {
	tracker.markEvaluated(resp)
	decision := s.decideErrorHandlingRule(ctx, tracker, account, resp.StatusCode, respBody, reqModel, errorHandlingRuleDecisionOptions{
		Attempt: attempt, RetryStart: retryStart,
	})
	if !decision.Matched {
		return errorHandlingRuleOutcomeNone, nil, nil
	}
	s.logErrorHandlingRuleDecision(ctx, c, account, resp.StatusCode, resp.Header, respBody, attempt, passthroughPath, decision)

	switch decision.EffectiveAction {
	case ErrorHandlingActionPassthrough:
		return errorHandlingRuleOutcomeDone, nil, s.writeErrorHandlingRulePassthrough(ctx, c, resp, respBody, account, reqModel)
	case ErrorHandlingActionFailover:
		return errorHandlingRuleOutcomeDone, nil, s.errorHandlingRuleFailover(ctx, resp, respBody, account, reqModel, decision, false)
	case ErrorHandlingActionRetry:
		if decision.RetryDelay > 0 {
			if err := sleepWithContext(ctx, decision.RetryDelay); err != nil {
				return errorHandlingRuleOutcomeDone, nil, err
			}
		}
		return errorHandlingRuleOutcomeRetry, nil, nil
	default:
		gatewayLog(ctx).Warn("error_handling_rule_unknown_action",
			zap.String("rule_id", decision.RuleID), zap.String("rule_action", decision.ConfiguredAction))
		return errorHandlingRuleOutcomeNone, nil, nil
	}
}

func (s *GatewayService) logErrorHandlingRuleDecision(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	statusCode int,
	respHeader http.Header,
	respBody []byte,
	attempt int,
	passthroughPath bool,
	decision errorHandlingRuleDecision,
) {
	if !decision.Matched {
		return
	}

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusCode,
		UpstreamRequestID:  respHeader.Get("x-request-id"),
		Passthrough:        passthroughPath,
		Kind:               "error_handling_rule_" + decision.EffectiveAction,
		Message:            extractUpstreamErrorMessage(respBody),
		Detail: func() string {
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
			}
			return ""
		}(),
	})

	// 触发日志：走 gatewayLog(ctx)，trace_id / request_id / client_request_id 由
	// request-scoped logger 自动带上（业务侧重复绑会产生重复 JSON key，见
	// gateway_logging_baseline_test.go）。字段命名对齐既有网关日志：上游的
	// x-request-id 是 upstream_request_id，不是 request_id。
	//
	// 级别取 warn 而不是 info，有两个硬约束：OpsSystemLogSink.shouldIndex 只把
	// warn+ 写进 ops_system_logs；生产把 log.level 调到 warn 时 info 行会被 zap
	// 直接丢掉，Vector 也就没得采。引擎只在上游报错时才触发，warn 不会刷屏。
	fields := []zap.Field{
		zap.String("rule_id", decision.RuleID),
		zap.String("rule_name", decision.RuleName),
		zap.String("rule_action", decision.ConfiguredAction),
		zap.String("outcome", decision.EffectiveAction),
		zap.String("exhausted_action", decision.ExhaustedAction),
		zap.Int("upstream_status_code", statusCode),
		zap.String("upstream_request_id", respHeader.Get("x-request-id")),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.Bool("passthrough_path", passthroughPath),
		zap.Int("attempt", attempt),
		zap.Duration("retry_elapsed", decision.RetryElapsed),
		zap.String("upstream_message", truncateString(sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody)), 256)),
	}
	if decision.ConfiguredAction == ErrorHandlingActionRetry {
		fields = append(fields, zap.Int("rule_retry_used", decision.RetryUsed), zap.Int("rule_retry_limit", decision.RetryLimit))
	}
	if decision.DowngradeReason != "" {
		fields = append(fields, zap.String("downgrade_reason", decision.DowngradeReason))
	}
	gatewayLog(ctx).Warn("error_handling_rule_matched", fields...)
}

// writeErrorHandlingRulePassthrough commits the exact upstream status and body.
// It intentionally does not use handleErrorResponse: that helper rewrites most
// statuses and can turn account side effects into a failover error.
func (s *GatewayService) writeErrorHandlingRulePassthrough(ctx context.Context, c *gin.Context, resp *http.Response, respBody []byte, account *Account, reqModel string) error {
	scheduleOllamaCloudUsageActivity(s.deferredService, account)
	if s.rateLimitService != nil {
		// Preserve account health/rate-limit bookkeeping, but passthrough semantics
		// always win even if the account was disabled as a side effect.
		_ = s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, reqModel)
	}

	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
	// 与其他「已提交响应」的终态出口对齐：ops_error_logs 的顶层上游字段靠这一步填，
	// 漏掉的话 Ops 后台看不到这些请求的上游状态码/错误消息。
	detail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		detail = truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, message, detail)

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	MarkResponseCommitted(c)
	c.Data(resp.StatusCode, contentType, respBody)

	if message == "" {
		return fmt.Errorf("upstream error: %d (error handling rule passthrough)", resp.StatusCode)
	}
	return fmt.Errorf("upstream error: %d (error handling rule passthrough) message=%s", resp.StatusCode, message)
}

func (s *GatewayService) errorHandlingRuleFailover(
	ctx context.Context,
	resp *http.Response,
	respBody []byte,
	account *Account,
	reqModel string,
	decision errorHandlingRuleDecision,
	streamRule bool,
) error {
	restoreErrorHandlingRuleBody(resp, respBody)
	s.handleFailoverSideEffects(ctx, resp, account, reqModel)
	errType, errMessage := safeAnthropicError(respBody)
	retryableOnSameAccount := account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode)
	nextAccountAction := NextAccountLegacyRetry
	if streamRule {
		// SSE rule retries are fully owned by retry_count inside the service attempt
		// loop. Pool-mode retries must not stack when the rule switches account.
		retryableOnSameAccount = false
		nextAccountAction = NextAccountRetry
	}
	return &UpstreamFailoverError{
		StatusCode:             resp.StatusCode,
		ResponseBody:           respBody,
		ResponseHeaders:        resp.Header.Clone(),
		RetryableOnSameAccount: retryableOnSameAccount,
		NextAccountAction:      nextAccountAction,
		ErrorRuleID:            decision.RuleID,
		ExhaustedAction:        decision.ExhaustedAction,
		SafeErrorType:          errType,
		SafeErrorMessage:       errMessage,
	}
}
