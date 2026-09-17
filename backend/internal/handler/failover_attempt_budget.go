package handler

import (
	"go.uber.org/zap"
)

// defaultMaxUpstreamAttempts 是单次请求上游尝试总预算的兜底默认值，
// 与 config 的 gateway.max_upstream_attempts 默认值保持一致；仅在调用方
// 没有配置（cfg 为空）时使用。
const defaultMaxUpstreamAttempts = 12

// requestAttemptBudget 是单次请求的上游尝试总预算。
//
// 存在的理由：既有闸门各自只管一个维度——同账号原地重试受
// pool_mode_retry_count / 规则 RetryLimit 约束，换号受 MaxAccountSwitches
// 约束，OAuth 429 还有自己的时间窗口。任何一个维度单独看都是有界的，
// 但乘起来（以及 OAuth 429 窗口过期后重开）可以让单次请求打出几十次真实
// 上游调用、被拖到十几分钟（#248）。本预算是唯一横跨全部推进路径的计数：
// 无论下一次尝试来自原地重试、换号还是 OAuth 429 窗口，都消耗同一个额度。
//
// 零值可用，表示不限（limit == 0），即保持接入本预算之前的行为。
type requestAttemptBudget struct {
	limit int
	used  int
}

// newRequestAttemptBudget 归一化预算：非正数一律存成 0，使 Limit() 与日志里
// 的 max_upstream_attempts 始终只有「0=不限」一种表达。
func newRequestAttemptBudget(limit int) requestAttemptBudget {
	if limit < 0 {
		limit = 0
	}
	return requestAttemptBudget{limit: limit}
}

// Consume 记录一次已经失败的上游尝试，返回 false 表示本次请求的总预算已耗尽、
// 调用方不得再推进（既不能原地重试也不能换号），应按 failover 耗尽终止。
//
// 计数口径是「已发生的上游尝试」，因此 limit=N 意味着单次请求最多 N 次上游
// 调用（对应 ops_error_logs.upstream_errors 最多 N 个元素）。
// nil 接收者表示「未装配预算」，一律放行。
func (b *requestAttemptBudget) Consume() bool {
	if b == nil {
		return true
	}
	b.used++
	if b.limit <= 0 {
		return true
	}
	return b.used < b.limit
}

// Used 返回本次请求已消耗的上游尝试数（供日志使用）。
func (b *requestAttemptBudget) Used() int {
	if b == nil {
		return 0
	}
	return b.used
}

// Limit 返回本次请求的上游尝试预算（0 表示不限）。
func (b *requestAttemptBudget) Limit() int {
	if b == nil {
		return 0
	}
	return b.limit
}

// logAttemptBudgetExhausted 统一预算耗尽日志的字段口径。event 前缀由各路径
// 自己给，与同一循环里其它 failover 日志的命名保持一致。
func logAttemptBudgetExhausted(log *zap.Logger, event string, accountID int64, upstreamStatus int, budget *requestAttemptBudget) {
	if log == nil {
		return
	}
	log.Warn(event,
		zap.Int64("account_id", accountID),
		zap.Int("upstream_status", upstreamStatus),
		zap.Int("upstream_attempts", budget.Used()),
		zap.Int("max_upstream_attempts", budget.Limit()),
	)
}

// maxUpstreamAttemptsFromConfig 解析配置值。cfgPresent 为 false（没有配置对象）
// 时回退默认值；有配置对象时显式的 0/负数按「不限」处理，保留现状逃生阀。
func maxUpstreamAttemptsFromConfig(configured int, cfgPresent bool) int {
	if !cfgPresent {
		return defaultMaxUpstreamAttempts
	}
	if configured <= 0 {
		return 0
	}
	return configured
}
