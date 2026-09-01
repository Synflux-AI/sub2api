package service

import (
	"context"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// modelRPMSkippedNoModelTotal 统计「资格检查时 ctx 里没有公开模型名，模型限流被跳过」的次数。
//
// 这是方案 A（从 ctx 取模型名）的兜底观测点：以后新增 handler 若忘了在检查前
// setOpsRequestContext，或把 ctx 快照取在 setOpsRequestContext 之前，
// 都会静默漏掉模型限流。计数器 + 采样日志让这种漏网可被发现。
var (
	modelRPMSkippedNoModelTotal atomic.Int64
	modelRPMSkippedLogCounter   atomic.Int64
)

// modelRPMSkippedLogInterval 采样间隔：无模型的端点（音频、web search）本就常态命中，
// 每次都打日志只会淹没有价值的信号。
const modelRPMSkippedLogInterval = 256

// ModelRPMSkippedNoModelCount 返回因取不到模型名而跳过模型限流的累计次数。
func ModelRPMSkippedNoModelCount() int64 {
	return modelRPMSkippedNoModelTotal.Load()
}

// WithoutModelRPMLimit 标记该 ctx 派生的资格检查跳过模型维度 RPM 限流。
// 用于同一客户端请求内的二次资格检查，避免重复消耗模型配额。
func WithoutModelRPMLimit(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.SkipModelRPMLimit, true)
}

func modelRPMLimitSkipped(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	skip, _ := ctx.Value(ctxkey.SkipModelRPMLimit).(bool)
	return skip
}

// requestModelForRPM 取客户端请求体里的公开模型名（由 setOpsRequestContext 写入 request context）。
//
// 选 ctxkey.Model 而非 ctxkey.RequestedPublicModel：后者只在 composite 分组解析路由时设置，
// 覆盖面太窄；前者各调用点传入的都是 parsedReq.Model 一类的客户端原始模型名。
func requestModelForRPM(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(ctxkey.Model).(string)
	return NormalizeModelRPMPattern(model)
}

// checkModelRPM 执行模型维度 RPM 限流。
//
// 命中的规则全部生效，任一超限即拒；按「target 具体度降序、id 升序」确定性排序后
// 逐条 INCR 并比较，首次超限即返回。早返回意味着被拒的请求不再计入更宽范围的桶——
// 这是对的，请求根本没打到上游。
//
// 已知不覆盖：WebSocket 长连接（ResponsesWebSocket / GrokRealtime）只在握手时查一次资格，
// 后续每轮切换模型不复查，因此一条长连接可以在被限流的模型上继续跑。本期不处理。
//
// Redis 出错一律 fail-open，与现有 RPM 行为一致。
func (s *BillingCacheService) checkModelRPM(ctx context.Context, user *User, group *Group) error {
	if s == nil || s.modelRPMRules == nil || s.modelRPMCache == nil || user == nil {
		return nil
	}
	if modelRPMLimitSkipped(ctx) {
		return nil
	}

	model := requestModelForRPM(ctx)
	if model == "" {
		modelRPMSkippedNoModelTotal.Add(1)
		if modelRPMSkippedLogCounter.Add(1)%modelRPMSkippedLogInterval == 0 {
			logger.LegacyPrintf(
				"service.billing_cache",
				"Warning: model rpm limit skipped, no model in request context (user=%d, sampled 1/%d, total=%d)",
				user.ID, modelRPMSkippedLogInterval, modelRPMSkippedNoModelTotal.Load(),
			)
		}
		return nil
	}

	rules := s.modelRPMRules.Snapshot(ctx)
	if len(rules) == 0 {
		return nil
	}

	matched := make([]ModelRPMRule, 0, 2)
	for _, rule := range rules {
		if !rule.Enabled || !rule.MatchesModel(model) || !rule.MatchesTarget(user.ID, group) {
			continue
		}
		matched = append(matched, rule)
	}
	if len(matched) == 0 {
		return nil
	}

	// 分钟戳只取一次：既省 RTT，也避免同一请求的多条规则跨分钟边界落进不同窗口。
	minute, err := s.modelRPMCache.MinuteTimestamp(ctx)
	if err != nil {
		logger.LegacyPrintf(
			"service.billing_cache",
			"Warning: model rpm minute timestamp failed for user=%d model=%s: %v",
			user.ID, model, err,
		)
		return nil // fail-open
	}

	for _, rule := range matched {
		bucketUserID := int64(0)
		if rule.Scope == ModelRPMScopeUser {
			bucketUserID = user.ID
		}
		count, incErr := s.modelRPMCache.IncrementRuleRPM(ctx, rule.ID, bucketUserID, minute)
		if incErr != nil {
			logger.LegacyPrintf(
				"service.billing_cache",
				"Warning: model rpm increment failed for rule=%d user=%d model=%s: %v",
				rule.ID, user.ID, model, incErr,
			)
			continue // fail-open：单条规则失败不影响其余规则
		}
		if count > rule.RPMLimit {
			return ErrModelRPMExceeded
		}
	}

	return nil
}
