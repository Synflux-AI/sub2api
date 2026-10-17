package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// stampForwardResponseOutcome 在提交异步记账前，从请求上下文补齐最后一次 Forward
// 尝试的上游结束原因观测，并记下客户端是否中途断开；记账 worker 拿不到 *gin.Context。
func stampForwardResponseOutcome(c *gin.Context, result *service.ForwardResult) {
	if result == nil {
		return
	}
	if result.ResponseOutcome == nil {
		result.ResponseOutcome = service.ResponseOutcomeFromContext(c)
	}
	if result.ResponseOutcome != nil && result.ClientDisconnect {
		result.ResponseOutcome.ClientDisconnected = true
	}
}

// stampOpenAIResponseOutcome 同上；WS 轮次已由转发层按轮写入，不会被覆盖。
func stampOpenAIResponseOutcome(c *gin.Context, result *service.OpenAIForwardResult) {
	if result == nil {
		return
	}
	if result.ResponseOutcome == nil {
		result.ResponseOutcome = service.ResponseOutcomeFromContext(c)
	}
	if result.ResponseOutcome != nil && result.ClientDisconnect {
		result.ResponseOutcome.ClientDisconnected = true
	}
}
