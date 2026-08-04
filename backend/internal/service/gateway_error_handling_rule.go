package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type errorHandlingRuleTracker struct {
	ruleID  string
	retries int

	// evaluated 记录最近一次被规则引擎评估过的响应对象。转发主循环里有若干条
	// 从 400 分支内部 break 出去的路径，它们带出的响应从没经过任何接入点，
	// 循环后的兜底靠这个指针区分「已评估过」和「漏网的」，避免重复评估。
	evaluated *http.Response
}

func (t *errorHandlingRuleTracker) consume(ruleID string, retryLimit int) bool {
	if t.ruleID != ruleID {
		t.ruleID = ruleID
		t.retries = 0
	}
	if t.retries >= retryLimit {
		return false
	}
	t.retries++
	return true
}

func (t *errorHandlingRuleTracker) markEvaluated(resp *http.Response) {
	t.evaluated = resp
}

func (t *errorHandlingRuleTracker) alreadyEvaluated(resp *http.Response) bool {
	return t.evaluated != nil && t.evaluated == resp
}

type errorHandlingRuleOutcome int

const (
	errorHandlingRuleOutcomeNone errorHandlingRuleOutcome = iota
	errorHandlingRuleOutcomeRetry
	errorHandlingRuleOutcomeDone
)

func (s *GatewayService) errorHandlingRulesActive(ctx context.Context, account *Account) bool {
	if account == nil || account.Type != AccountTypeAPIKey || s == nil || s.settingService == nil {
		return false
	}
	settings := s.settingService.GetErrorHandlingRuleSettingsCached(ctx)
	return settings.Enabled && len(settings.Rules) > 0
}

func (s *GatewayService) matchErrorHandlingRuleForAccount(ctx context.Context, account *Account, statusCode int, respBody []byte) (*ErrorHandlingRule, int) {
	if account == nil || account.Type != AccountTypeAPIKey || s == nil || s.settingService == nil {
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

	if s.builtinSignatureHandlingOwns(ctx, account, resp.StatusCode, respBody, reqModel) {
		return errorHandlingRuleOutcomeNone, nil, nil
	}

	rule, retryLimit := s.matchErrorHandlingRuleForAccount(ctx, account, resp.StatusCode, respBody)
	if rule == nil {
		return errorHandlingRuleOutcomeNone, nil, nil
	}

	// 先把动作定下来再执行，日志才能一条讲清「配的是什么」和「实际做了什么」。
	action := rule.Action
	retryUsed := 0
	downgrade := ""
	elapsed := time.Since(retryStart)
	if action == ErrorHandlingActionRetry {
		switch {
		case !tracker.consume(rule.ID, retryLimit):
			action, downgrade = ErrorHandlingActionFailover, "retry_budget_exhausted"
			retryUsed = retryLimit
		case elapsed >= maxRetryElapsed:
			action, downgrade = ErrorHandlingActionFailover, "retry_window_elapsed"
			retryUsed = tracker.retries
		case attempt >= maxRetryAttempts:
			action, downgrade = ErrorHandlingActionFailover, "max_attempts_reached"
			retryUsed = tracker.retries
		default:
			retryUsed = tracker.retries
		}
	}

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Passthrough:        passthroughPath,
		Kind:               "error_handling_rule_" + action,
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
		zap.String("rule_id", rule.ID),
		zap.String("rule_name", rule.Name),
		zap.String("rule_action", rule.Action),
		zap.String("outcome", action),
		zap.Int("upstream_status_code", resp.StatusCode),
		zap.String("upstream_request_id", resp.Header.Get("x-request-id")),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.Bool("passthrough_path", passthroughPath),
		zap.Int("attempt", attempt),
		zap.Duration("retry_elapsed", elapsed),
		zap.String("upstream_message", truncateString(sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody)), 256)),
	}
	if rule.Action == ErrorHandlingActionRetry {
		fields = append(fields, zap.Int("rule_retry_used", retryUsed), zap.Int("rule_retry_limit", retryLimit))
	}
	if downgrade != "" {
		fields = append(fields, zap.String("downgrade_reason", downgrade))
	}
	gatewayLog(ctx).Warn("error_handling_rule_matched", fields...)

	switch action {
	case ErrorHandlingActionPassthrough:
		return errorHandlingRuleOutcomeDone, nil, s.writeErrorHandlingRulePassthrough(ctx, c, resp, respBody, account, reqModel)
	case ErrorHandlingActionFailover:
		return errorHandlingRuleOutcomeDone, nil, s.errorHandlingRuleFailover(ctx, resp, respBody, account, reqModel)
	case ErrorHandlingActionRetry:
		delay := retryBackoffDelay(attempt)
		if remaining := maxRetryElapsed - elapsed; delay > remaining {
			delay = remaining
		}
		if delay > 0 {
			if err := sleepWithContext(ctx, delay); err != nil {
				return errorHandlingRuleOutcomeDone, nil, err
			}
		}
		return errorHandlingRuleOutcomeRetry, nil, nil
	default:
		// matchErrorHandlingRule 已经过滤掉未知 action，这里是兜底：绝不能让未知
		// 动作掉进 retry 分支——那条路不消耗重试计数，等于无上限原地重试。
		gatewayLog(ctx).Warn("error_handling_rule_unknown_action",
			zap.String("rule_id", rule.ID), zap.String("rule_action", rule.Action))
		return errorHandlingRuleOutcomeNone, nil, nil
	}
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

func (s *GatewayService) errorHandlingRuleFailover(ctx context.Context, resp *http.Response, respBody []byte, account *Account, reqModel string) error {
	restoreErrorHandlingRuleBody(resp, respBody)
	s.handleFailoverSideEffects(ctx, resp, account, reqModel)
	return &UpstreamFailoverError{
		StatusCode:             resp.StatusCode,
		ResponseBody:           respBody,
		RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
	}
}
