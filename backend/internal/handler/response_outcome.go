package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// stampForwardResponseOutcome 在提交异步记账前，从请求上下文补齐最后一次 Forward
// 尝试的上游结束原因观测；记账 worker 拿不到 *gin.Context。
func stampForwardResponseOutcome(c *gin.Context, result *service.ForwardResult) {
	if result == nil || result.ResponseOutcome != nil {
		return
	}
	result.ResponseOutcome = service.ResponseOutcomeFromContext(c)
}

// stampOpenAIResponseOutcome 同上；WS 轮次已由转发层按轮写入，不会被覆盖。
func stampOpenAIResponseOutcome(c *gin.Context, result *service.OpenAIForwardResult) {
	if result == nil || result.ResponseOutcome != nil {
		return
	}
	result.ResponseOutcome = service.ResponseOutcomeFromContext(c)
}
