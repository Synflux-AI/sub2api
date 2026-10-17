package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/responseoutcome"
	"github.com/gin-gonic/gin"
)

// usageFinishReasonPtr 把观测快照汇总成落库的标准化原因；未采集时为 nil。
func usageFinishReasonPtr(outcome *responseoutcome.Outcome) *string {
	if outcome == nil {
		return nil
	}
	reason := outcome.FinishReason()
	if reason == "" {
		return nil
	}
	return &reason
}

// observeAnthropicResponseOutcome 只喂入结束原因观测，不触碰上游模型/服务层级观测，
// 供自行解析 Anthropic 事件的兼容转换路径在转换前采集上游语义。
func observeAnthropicResponseOutcome(c *gin.Context, payload []byte) {
	upstreamResponseModelObserverFromContext(c).outcomeObserver().ObserveAnthropicPayload(payload)
}

// observeOpenAIResponseOutcome 同上，用于 OpenAI 载荷。
func observeOpenAIResponseOutcome(c *gin.Context, payload []byte, eventType string) {
	upstreamResponseModelObserverFromContext(c).outcomeObserver().ObserveOpenAIPayload(payload, eventType)
}
