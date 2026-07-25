package service

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
)

// injectTraceHeader 在账号开启 trace_id_passthrough 时，为出站上游请求注入 X-Trace-Id。
// 在客户端 header 白名单拷贝之后调用，确保覆盖客户端可能自带的同名头；
// 放在 ApplyHeaderOverrides / ApplyCustomHeaders 之前，使管理员的显式覆写仍能胜出。
//
// ctx 必须是承载入站请求的原始 context（由 middleware.RequestLogger 写入 ctxkey.TraceID），
// 不要传 detachUpstreamContext 派生出的 upstream context 之外的其他 context。
// 账号未开启开关时不写入任何头（Global Constraint 11：默认关闭）。
func injectTraceHeader(ctx context.Context, req *http.Request, account *Account) {
	if req == nil || ctx == nil || !account.IsTraceIDPassthroughEnabled() {
		return
	}
	raw, _ := ctx.Value(ctxkey.TraceID).(string)
	if traceID, ok := traceid.Normalize(raw); ok {
		req.Header.Set(traceid.Header, traceID)
	}
}
