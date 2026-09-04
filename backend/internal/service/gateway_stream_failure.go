package service

import (
	"context"
	"strconv"

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
	clientDisconnected bool,
	passthrough bool,
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
		zap.Bool("passthrough", passthrough),
		zap.Bool("client_disconnected", clientDisconnected),
		zap.String("message", safeMessage),
	)

	if c == nil {
		return
	}
	// Stage 借用来打「客户端已先行断开」的标：missing_terminal 分支不因客户端
	// 断开而提前 return（详见调用点注释），会与上游真断流的样本混在同一个
	// cause 桶里。这里只打标不改控制流，供 OpenObserve 按 stage 拆分统计。
	stage := ""
	if clientDisconnected {
		stage = "client_disconnected"
	}
	setOpsUpstreamError(c, 0, safeMessage, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:    account.Platform,
		AccountID:   account.ID,
		AccountName: account.Name,
		Passthrough: passthrough,
		Kind:        opsUpstreamErrorKindStreamFailure,
		Reason:      string(cause),
		Scope:       scope,
		Stage:       stage,
		Message:     safeMessage,
	})
}

const opsUpstreamErrorKindStreamErrorUnmatched = "stream_error_unmatched"

// unmatchedStreamErrorEvent 是一次「上游发了 error 帧、但错误处理规则没匹配上」
// 的现场。运维配关键字全靠这份原文——现状是它被完全丢弃，只能靠持续监控猜。
type unmatchedStreamErrorEvent struct {
	statusCode int
	errType    string
	message    string
	detail     string
}

// flushUnmatchedStreamError 在 attempt 终态落一条未命中记录。
//
// 采用 hit-priority 去重：调用方保证同一 attempt 内只保留首条未命中，且一旦
// 后续 error 命中规则就整体丢弃——同一次故障只应产生一条记录，命中记录
// （error_handling_rule_matched）优先级更高。
func (s *GatewayService) flushUnmatchedStreamError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	model string,
	attempt int,
	ev *unmatchedStreamErrorEvent,
) {
	if ev == nil || account == nil {
		return
	}
	// logMessage 只喂给 stdout 结构化 warn，必须是脱敏后的值——这条日志不经过
	// appendOpsUpstreamError，没有别的机会脱敏。
	logMessage := sanitizeUpstreamErrorPayload(ev.message)

	// opsDetail 喂给 ops 事件，必须保留原文（未脱敏）：appendOpsUpstreamError
	// 内部会从 Detail/Message 算出 rawMatchBody 供规则匹配用，脱敏必须在那
	// 之后才发生，否则关键字会被 *** 打断（Finding 2）。这里只做「原文 →
	// 截断」，脱敏交给 appendOpsUpstreamError 内部的兜底逻辑。
	//
	// 隐私开关默认 fail-closed：s 或 s.cfg 为 nil 时 opsDetail 保持空，而不是
	// 保留完整未截断的上游正文（Finding 3）。
	opsDetail := ""
	if s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		opsDetail = truncateString(ev.detail, maxBytes)
	}

	logger.FromContext(ctx).Warn("gateway.stream_error_event_unmatched",
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("model", model),
		zap.Int("attempt", attempt),
		zap.Int("status_code", ev.statusCode),
		zap.String("error_type", ev.errType),
		zap.String("message", logMessage),
	)

	if c == nil {
		return
	}
	// UpstreamStatusCode 故意不写（传 0）：这里的 statusCode 是从上游 SSE error
	// 帧推断出来的合成值（anthropicPassthroughSSEError 恒非零），wire 上游状态
	// 其实是 200。非零值会让 checkSkipMonitoringForUpstreamEvent 提前退出的
	// 短路失效，进而对一条纯观测记录跑 MatchRule；只要运维配了一条仅凭状态码
	// （不含关键字）+ skip_monitoring=true 的 passthrough 规则，就会整条丢弃
	// ops_error_logs 行，包括这里刚写的 upstream_error_message（Finding 1）。
	// 派生出来的状态码改放进 Reason，形如 http_500。
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:    account.Platform,
		AccountID:   account.ID,
		AccountName: account.Name,
		Passthrough: true,
		Kind:        opsUpstreamErrorKindStreamErrorUnmatched,
		Reason:      "http_" + strconv.Itoa(ev.statusCode),
		Message:     ev.message,
		Detail:      opsDetail,
	})
}
