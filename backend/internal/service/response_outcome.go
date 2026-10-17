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

// usageResponseOutcomeForStorage 决定原始事实是否随用量行落库。正常结束（stop / tool_use）
// 的事实几乎都能由 finish_reason 推出，usage_logs 是写入最频繁的大表，因此不写；
// 但客户端中途断开属于传输层事实，仍保留。
func usageResponseOutcomeForStorage(outcome *responseoutcome.Outcome) *responseoutcome.Outcome {
	if outcome == nil {
		return nil
	}
	switch outcome.FinishReason() {
	case responseoutcome.ReasonStop, responseoutcome.ReasonToolUse:
		if !outcome.ClientDisconnected {
			return nil
		}
	}
	return outcome
}
