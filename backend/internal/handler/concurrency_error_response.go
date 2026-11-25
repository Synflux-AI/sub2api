package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
)

const statusClientClosedRequest = 499

const (
	gatewayQueueFullCode        = "gateway_queue_full"
	gatewayConcurrencyLimitCode = "gateway_concurrency_limit"
)

func concurrencyErrorResponse(err error, slotType string) (int, string, string, string) {
	var waitQueueFullErr *WaitQueueFullError
	if errors.As(err, &waitQueueFullErr) {
		return http.StatusTooManyRequests, "rate_limit_error", gatewayQueueFullCode,
			"Too many pending requests, please retry later"
	}

	var concurrencyErr *ConcurrencyError
	if errors.As(err, &concurrencyErr) {
		if concurrencyErr.SlotType != "" {
			slotType = concurrencyErr.SlotType
		}
		return http.StatusTooManyRequests, "rate_limit_error", gatewayConcurrencyLimitCode,
			fmt.Sprintf("Concurrency limit exceeded for %s, please retry later", slotType)
	}

	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "api_error", "", "context canceled"
	}

	return http.StatusServiceUnavailable, "api_error", "", "Service temporarily unavailable, please retry later"
}

// markLocalGatewayError 标记本次错误响应由网关自身生成（计费/额度/限额/
// RPM/用户并发等），供前置反向代理区分本地错误与上游原样错误。
// 只能在写出网关自身错误体之前调用，不能用于转发上游响应体的路径。
func markLocalGatewayError(c *gin.Context) {
	if c == nil || c.Writer == nil {
		return
	}
	httputil.MarkLocalError(c.Writer.Header())
}

// markLocalUserSlotError 只对用户维度的并发/排队拒绝打本地标记：
// 这是调用方自己的并发上限，提示应原样到达客户端；账号维度的并发属于
// 平台侧容量，不打标记。
func markLocalUserSlotError(c *gin.Context, err error, slotType string) {
	var waitQueueFullErr *WaitQueueFullError
	var concurrencyErr *ConcurrencyError
	switch {
	case errors.As(err, &waitQueueFullErr):
		if waitQueueFullErr.SlotType != "" {
			slotType = waitQueueFullErr.SlotType
		}
	case errors.As(err, &concurrencyErr):
		if concurrencyErr.SlotType != "" {
			slotType = concurrencyErr.SlotType
		}
	default:
		return
	}
	if slotType == "user" {
		markLocalGatewayError(c)
	}
}
