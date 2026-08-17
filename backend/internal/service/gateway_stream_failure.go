package service

import (
	"context"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// streamFailureCause 表达「上游没把流跑完」的结构性原因。
// 取值由代码位置决定，不靠解析文案——文案会随上游变化，代码位置不会。
type streamFailureCause string

const (
	// streamFailureMissingTerminal：流干净关闭但没有 message_stop 等终止事件。
	streamFailureMissingTerminal streamFailureCause = "missing_terminal_event"
	// streamFailureReadError：读取上游流时报错。
	streamFailureReadError streamFailureCause = "read_error"
	// streamFailureIntervalTimeout：两次数据之间超过配置的间隔上限。
	streamFailureIntervalTimeout streamFailureCause = "interval_timeout"
)

const opsUpstreamErrorKindStreamFailure = "stream_failure"

const (
	// 断流发生在首个语义事件之前 —— 客户端零字节，理论上可干净重试。
	streamFailureScopeBeforeFirstToken = "before_first_token"
	// 断流发生在首字之后 —— 重试会腐化流，只能观测。
	streamFailureScopeAfterFirstToken = "after_first_token"
)

// recordStreamFailureCause 把流中断的真实原因写进两个通道，不改变对客响应：
//
//   - stdout 结构化 warn：计数通道。请求经规则重试后最终成功时不会写
//     ops_error_logs 行，只有这里能拿到完整计数。
//   - upstream_error_message + upstream_errors：后台可搜索通道。
//
// upstream_status_code 故意不写（传 0）：wire 上游状态确实是 200，合成一个
// 5xx 会污染上游状态维度；cause 由 OpsUpstreamErrorEvent.Reason 表达。传 0
// 同时让 checkSkipMonitoringForUpstreamEvent 提前返回，不会误触发
// skip_monitoring 规则匹配。
func (s *GatewayService) recordStreamFailureCause(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	model string,
	cause streamFailureCause,
	message string,
	firstTokenSeen bool,
) {
	if account == nil {
		return
	}
	scope := streamFailureScopeBeforeFirstToken
	if firstTokenSeen {
		scope = streamFailureScopeAfterFirstToken
	}

	safeMessage := sanitizeUpstreamErrorPayload(message)

	logger.FromContext(ctx).Warn("gateway.stream_failure",
		zap.String("cause", string(cause)),
		zap.String("scope", scope),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("model", model),
		zap.Bool("passthrough", true),
		zap.String("message", safeMessage),
	)

	if c == nil {
		return
	}
	setOpsUpstreamError(c, 0, safeMessage, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:    account.Platform,
		AccountID:   account.ID,
		AccountName: account.Name,
		Passthrough: true,
		Kind:        opsUpstreamErrorKindStreamFailure,
		Reason:      string(cause),
		Scope:       scope,
		Message:     safeMessage,
	})
}
