package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 网关转发链路的日志 component。与 logger.CtxPrintf 首参传的字面量保持一致，
// 这样同一条链路上 printf 风格和结构化两种日志在 OpenObserve 里归到同一个桶。
const (
	componentGateway       = "service.gateway"
	componentOpenAIGateway = "service.openai_gateway"
)

// gatewayLog 返回带 component=service.gateway 的 request-scoped logger。
//
// 为什么不直接用 logger.C(ctx)：#103 的验收口径是按 component 统计 trace_id 覆盖率
// （SELECT component, count(*), count(trace_id) FROM sub2api GROUP BY component），
// 裸的 logger.C(ctx) 不带 component，日志会落进空 component 桶，
// service.gateway 的覆盖率永远看不到从 0% 涨到 100%；
// 同时 ops_system_log_sink 会把它们记成 component="app"（见 ops_repo.go 的
// `if component == "" { component = "app" }`），Ops 后台按 component 筛也筛不出来。
//
// .With() 不增加包装层，caller 仍指向业务调用点。
func gatewayLog(ctx context.Context) *zap.Logger {
	return logger.C(ctx).With(zap.String("component", componentGateway))
}

// openaiGatewayLog 是 gatewayLog 的 OpenAI 网关版本，理由同上。
func openaiGatewayLog(ctx context.Context) *zap.Logger {
	return logger.C(ctx).With(zap.String("component", componentOpenAIGateway))
}
