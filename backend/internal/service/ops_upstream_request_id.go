package service

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// opsUpstreamRequestIDs 是单个网关请求内的上游请求标识记录器，挂在请求 context 上。
// 错误日志按账户配置的头名（与用量明细的“上游ID”同一口径）取直接上游的请求标识：
// 选中账户时登记头名，HTTP 上游每次返回时按头名记下该账户最近一次响应的标识，
// appendOpsUpstreamError 在汇聚点取用，无需逐个改写各转发路径的错误事件。
type opsUpstreamRequestIDs struct {
	mu          sync.Mutex
	headerNames map[int64]string
	lastIDs     map[int64]string
}

type opsUpstreamRequestIDsCtxKey struct{}

// WithOpsUpstreamRequestIDCapture 为请求 context 挂上上游请求标识记录器。
// 已挂载时原样返回，避免嵌套中间件丢失已登记的状态。
func WithOpsUpstreamRequestIDCapture(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if opsUpstreamRequestIDsFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, opsUpstreamRequestIDsCtxKey{}, &opsUpstreamRequestIDs{})
}

func opsUpstreamRequestIDsFromContext(ctx context.Context) *opsUpstreamRequestIDs {
	if ctx == nil {
		return nil
	}
	rec, _ := ctx.Value(opsUpstreamRequestIDsCtxKey{}).(*opsUpstreamRequestIDs)
	return rec
}

// RegisterOpsAccountUpstreamRequestIDHeader 登记账户指定的上游请求标识头名；
// 账户未指定头名时不登记，错误事件保留各路径自带的标识。
func RegisterOpsAccountUpstreamRequestIDHeader(ctx context.Context, account *Account) {
	rec := opsUpstreamRequestIDsFromContext(ctx)
	if rec == nil || account == nil || account.ID <= 0 {
		return
	}
	name := UpstreamRequestIDHeaderName(account)
	if name == "" {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.headerNames == nil {
		rec.headerNames = make(map[int64]string)
	}
	rec.headerNames[account.ID] = name
}

// RecordOpsUpstreamResponse 记录一次 HTTP 上游尝试的结果。
// 请求失败或响应头里没有标识时清空该账户的记录，避免把上一次尝试的标识挂到本次错误上。
func RecordOpsUpstreamResponse(req *http.Request, accountID int64, resp *http.Response, err error) {
	if req == nil || accountID <= 0 {
		return
	}
	rec := opsUpstreamRequestIDsFromContext(req.Context())
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	name := rec.headerNames[accountID]
	if name == "" {
		return
	}
	id := ""
	if err == nil && resp != nil {
		id = strings.TrimSpace(resp.Header.Get(name))
	}
	if id == "" {
		delete(rec.lastIDs, accountID)
		return
	}
	if rec.lastIDs == nil {
		rec.lastIDs = make(map[int64]string)
	}
	rec.lastIDs[accountID] = id
}

// OpsUpstreamRequestIDFromContext 返回按账户配置头名取到的、该账户最近一次上游响应的请求标识。
func OpsUpstreamRequestIDFromContext(ctx context.Context, accountID int64) string {
	rec := opsUpstreamRequestIDsFromContext(ctx)
	if rec == nil || accountID <= 0 {
		return ""
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.lastIDs[accountID]
}
