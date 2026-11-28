package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	openAITransportReachabilityCooldown = 30 * time.Second
	openAITransportProxyAuthCooldown    = 10 * time.Minute
)

// openAITransportFailoverBody is the OpenAI-format error body attached to the
// failover error for a transport-level failure. Kept identical to the legacy
// inline 502 body so the client-visible payload is unchanged if failover is
// ultimately exhausted.
var openAITransportFailoverBody = []byte(`{"error":{"type":"upstream_error","message":"Upstream request failed"}}`)

// upstreamTransportErrorClass describes how to react to a transport-level upstream
// failure, either before response headers arrive or while reading the response body.
type upstreamTransportErrorClass uint8

const (
	upstreamTransportTransient upstreamTransportErrorClass = iota
	upstreamTransportReachability
	upstreamTransportProxyAuth
)

// Match specific reachability errors so a proxy timeout keeps its transient classification.
var upstreamReachabilityErrorMarkers = []string{
	"connection refused",
	"no route to host",
	"network is unreachable",
	"no such host",
}

// classifyUpstreamTransportError distinguishes reachability failures that can
// recover when the network returns from rejected proxy credentials that require
// operator intervention. Prefer typed errors and fall back to transport messages.
func classifyUpstreamTransportError(err error) upstreamTransportErrorClass {
	if err == nil {
		return upstreamTransportTransient
	}

	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return upstreamTransportReachability
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return upstreamTransportReachability
	}

	// SOCKS5 and HTTP CONNECT report rejected credentials as plain errors.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "authentication failed") || strings.Contains(msg, "proxy authentication required") {
		return upstreamTransportProxyAuth
	}
	for _, marker := range upstreamReachabilityErrorMarkers {
		if strings.Contains(msg, marker) {
			return upstreamTransportReachability
		}
	}
	return upstreamTransportTransient
}

// isClientCanceledTransportError reports whether a transport-level failure was
// caused by the client disconnecting: the request context itself is canceled
// and the round-trip aborted with context.Canceled. Such a failure says nothing
// about the upstream, so it is not recorded as an Ops upstream error event.
func isClientCanceledTransportError(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && ctx != nil && errors.Is(ctx.Err(), context.Canceled)
}

// handleOpenAIUpstreamTransportError handles a transport-level upstream failure
// (proxy/DNS/TCP/TLS or an interrupted response body). It:
//  1. records the failure in Ops error logs (status 0, kind=request_error; there is
//     no complete usable HTTP response), except when the client disconnected
//     (see isClientCanceledTransportError);
//  2. temporarily unschedules the account (DB + in-memory) for reachability
//     failures and rejected proxy credentials, with a cooldown for each class;
//  3. returns an error that is *UpstreamFailoverError (so the handler fails over
//     to a healthy account) for all non-canceled errors, or a plain error for
//     context.Canceled (client gone — no failover, no eviction).
//
// It deliberately does NOT write to the response: the handler owns the response
// (failover, or a protocol-correct error once failover is exhausted).
//
// passthrough tags the Ops error event for the OpenAI passthrough forward path.
func (s *OpenAIGatewayService) handleOpenAIUpstreamTransportError(ctx context.Context, c *gin.Context, account *Account, err error, passthrough bool) error {
	return s.handleOpenAIUpstreamTransportErrorWithURL(ctx, c, account, err, passthrough, "", true)
}

// handleOpenAIUpstreamResponseBodyReadError promotes an interrupted non-streaming
// response body to the same observable, rule-aware failover used by round-trip
// transport errors. Size enforcement and cancellation remain terminal because
// they do not indicate a fault on the selected upstream account.
func (s *OpenAIGatewayService) handleOpenAIUpstreamResponseBodyReadError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	err error,
	passthrough bool,
	upstreamURL string,
) error {
	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}
	if account == nil || !shouldClassifyOpenAIUpstreamStreamReadError(err, ctx, requestCtx) {
		return err
	}
	return s.handleOpenAIUpstreamTransportErrorWithURL(ctx, c, account, err, passthrough, upstreamURL, true)
}

// handleOpenAIUpstreamTransportErrorWithURL is handleOpenAIUpstreamTransportError
// plus the upstream URL on the Ops error event.
//
// 两个入口而不是给既有函数加参数：handleOpenAIUpstreamTransportError 有 21 个调用点，
// 它们本来就不记 UpstreamURL，为了两个 images 站点去改 21 处签名不划算。
//
// ruleEligible 是 #228 §六 allowlist 在传输层的对应物：video_status / video_content
// 查询绑定到创建任务时选中的原账号（owner binding），不能切换账号，错误处理规则引擎
// 因此不得接触这两类查询的传输层失败——否则会出现"规则已接管 failover"的
// ops_error_logs 记录，而 owner binding 实际上仍会阻止真正换号，这正是 #228 评审
// 修订 §六要消灭的"假生效"。与 HTTP 响应侧的 endpoint.IsGenerationRequest() 允许清单
// 保持同一形状（允许清单而不是拒绝清单）：新增调用点必须显式传 true 才能接线规则
// 引擎，默认（复制一个已有调用点粗心漏改）落在"不接线"这一更安全的一侧。
func (s *OpenAIGatewayService) handleOpenAIUpstreamTransportErrorWithURL(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	err error,
	passthrough bool,
	upstreamURL string,
	ruleEligible bool,
) error {
	if isClientCanceledTransportError(ctx, err) {
		return err
	}
	safeErr := sanitizeUpstreamErrorMessage(err.Error())
	setOpsUpstreamError(c, 0, safeErr, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: 0,
		UpstreamURL:        upstreamURL,
		Passthrough:        passthrough,
		Kind:               "request_error",
		Message:            safeErr,
	})

	// Client disconnected: do NOT fail over to another account and do NOT evict
	// this one — the upstream never had a chance to exhibit a fault.
	if errors.Is(err, context.Canceled) || (errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return err
	}

	// Transport attempt reached the network path; count as Ollama Cloud / OpenCode Go activity.
	if s != nil {
		scheduleOllamaCloudUsageActivity(s.deferredService, account)
		scheduleOpenCodeGoUsageActivity(s.deferredService, account)
	}

	// 插件已把请求交给上游时，自动切换账号可能造成重复扣费或重复执行。
	var pluginErr *PluginTransportError
	if errors.As(err, &pluginErr) && pluginErr.RequestSent {
		return err
	}

	switch classifyUpstreamTransportError(err) {
	case upstreamTransportReachability:
		s.tempUnscheduleOpenAITransportError(ctx, account, safeErr, openAITransportReachabilityCooldown)
	case upstreamTransportProxyAuth:
		s.tempUnscheduleOpenAITransportError(ctx, account, safeErr, openAITransportProxyAuthCooldown)
	}

	// 错误处理规则最后问一次：合成 502 + OpenAI 形状错误体后交给引擎。命中就用规则版
	// 错误（可能是同号重试 / 换号 / 直接返回客户端），否则保持内置的「一律 failover」。
	// 放在副作用（停号）之后：规则只决定动作，不决定是否记账，与 Anthropic 侧同序。
	//
	// ruleEligible=false 的调用点（Grok video_status/video_content 查询）连问都不问：
	// 这不是"规则未命中"，是结构性不让规则引擎看到这条错误。
	if ruleEligible {
		if ruleErr := s.openAITransportErrorRuleOverride(ctx, c, account, safeErr); ruleErr != nil {
			return ruleErr
		}
	}

	return &UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: openAITransportFailoverBody,
	}
}

// tempUnscheduleOpenAITransportError marks an account temporarily unschedulable
// after a transport failure, both persistently (DB, survives restart)
// and in-memory (immediate scheduler effect before the DB/account cache propagates).
//
// Log semantics:
//   - "openai.account_temp_unscheduled_transport" — emitted ONLY after a
//     successful DB write (both in-memory + persisted).
//   - "openai.account_temp_unscheduled_transport_memory_only" — emitted when
//     accountRepo is nil (in-memory only; no persistence).
//   - "openai.account_temp_unscheduled_transport_failed" — DB write attempted
//     but returned an error.
func (s *OpenAIGatewayService) tempUnscheduleOpenAITransportError(ctx context.Context, account *Account, safeErr string, cooldown time.Duration) {
	if s == nil || account == nil {
		return
	}
	// 上游配置开关：完全关闭 OpenAI 传输错误冷却。
	if s.cfg != nil && s.cfg.Gateway.DisableOpenAITransportErrorCooldown {
		return
	}
	// 全局"禁止临时停止调度"开关（仅 Anthropic/OpenAI 账号生效；本路径上的
	// Grok 等其他平台账号由 guard 内的平台判断放行，保持原有行为）
	if tempUnschedDisabledSkip(ctx, account.Platform, account.ID, "openai_transport_error") {
		return
	}
	until := time.Now().Add(cooldown)
	reason := "upstream transport error (proxy/network): " + safeErr

	// Immediate in-memory block so this process skips the account until the
	// persisted cooldown is visible on the scheduling Account. Selection is
	// fail-open: empty snapshot/DB cooldown fields drop a stale local block.
	s.BlockAccountScheduling(account, until, "transport_error")

	if s.accountRepo == nil {
		// No DB configured — block is in-memory only; emit a distinct event so
		// operators are not misled into thinking the block survived a restart.
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.account_temp_unscheduled_transport_memory_only",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("platform", account.Platform),
			zap.Time("until", until),
			zap.String("reason", reason),
		)
		return
	}

	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAccountStateUpdateTimeout)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(bgCtx, account.ID, until, reason); err != nil {
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.account_temp_unscheduled_transport_failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return
	}

	// DB write succeeded: both in-memory and persisted.
	logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
		"openai.account_temp_unscheduled_transport",
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.Time("until", until),
		zap.String("reason", reason),
	)
}
