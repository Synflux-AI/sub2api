package service

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
)

// injectTraceHeader 在账号开启 trace_id_passthrough 时，为出站上游请求注入 X-Trace-Id。
// 在客户端 header 白名单拷贝之后调用，确保覆盖客户端可能自带的同名头。
// 落点位于 ApplyHeaderOverrides / ApplyCustomHeaders 之前，但这不代表管理员覆写能胜出：
// x-trace-id 已在 headerOverrideBlockedNames（前后端两侧）中，覆写根本无法保存。
// 原因是 header 覆写是账号级静态值，若允许覆写就会把该账号的所有请求钉死在同一个
// trace id 上，链路关联彻底失效——与 session_id 被拉黑的理由一致。
// 因此这里注入的值就是最终出站值，调整落点顺序不会改变行为。
//
// ctx 应传承载入站请求的原始 context（由 middleware.RequestLogger 写入 ctxkey.TraceID），
// 而不是 detachUpstreamContext 派生出的 upstream context。
// 后者用的是 context.WithoutCancel，只切断取消传播、value 仍可读，所以传它今天也能工作；
// 传原始 ctx 是意图更明确的写法（trace 来自入站链路，与上游生命周期无关），
// 且不会在 detach 实现变化时变成静默失效的 bug。
// 账号未开启开关时不写入任何头（Global Constraint 11：默认关闭）。
func injectTraceHeader(ctx context.Context, req *http.Request, account *Account) {
	if req == nil || ctx == nil || !account.IsTraceIDPassthroughEnabled() {
		return
	}
	raw, _ := ctx.Value(ctxkey.TraceID).(string)
	if traceID, ok := traceid.Normalize(raw); ok {
		// 注意：这里写的是 canonical 的 X-Trace-Id，而 Anthropic 侧白名单拷贝走
		// addHeaderRaw(resolveWireCasing(key)) 写原始（多为小写）key 且不去重
		// （header_util.go:108）。因此**切勿**把 x-trace-id 加进 allowedHeaders 等白名单，
		// 否则两种拼写会同时出现在 wire 上；若确有需要，须先改成
		// deleteHeaderAllForms + setHeaderRaw。
		req.Header.Set(traceid.Header, traceID)
	}
}
