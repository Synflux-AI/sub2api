import { describe, expect, it } from 'vitest'
import {
  USAGE_FINISH_REASON_ABNORMAL,
  buildUsageFinishReasonOptions,
  getUsageFinishReasonBadgeClass,
  getUsageFinishReasonLabel,
  isAbnormalUsageFinishReason,
} from '../usageFinishReason'

const translate = (key: string) => key

describe('usageFinishReason', () => {
  it('treats normal control flow and unknown as non-abnormal', () => {
    expect(isAbnormalUsageFinishReason('stop')).toBe(false)
    expect(isAbnormalUsageFinishReason('tool_use')).toBe(false)
    expect(isAbnormalUsageFinishReason('pause_turn')).toBe(false)
    expect(isAbnormalUsageFinishReason('unknown')).toBe(false)
    expect(isAbnormalUsageFinishReason(null)).toBe(false)
    expect(isAbnormalUsageFinishReason('refusal')).toBe(true)
    expect(isAbnormalUsageFinishReason('truncated')).toBe(true)
    expect(isAbnormalUsageFinishReason('interrupted')).toBe(true)
  })

  it('labels missing values as not collected instead of stop', () => {
    expect(getUsageFinishReasonLabel(null, translate)).toBe('usage.finishReasonNotCollected')
    expect(getUsageFinishReasonLabel('refusal', translate)).toBe('usage.finishReasons.refusal')
    expect(getUsageFinishReasonLabel('future_value', translate)).toBe('future_value')
  })

  it('uses distinct badge tones for refusal, truncation and stop', () => {
    expect(getUsageFinishReasonBadgeClass('refusal')).toContain('red')
    expect(getUsageFinishReasonBadgeClass('truncated')).toContain('amber')
    expect(getUsageFinishReasonBadgeClass('stop')).toContain('emerald')
  })

  it('builds options with all and abnormal shortcuts first', () => {
    const options = buildUsageFinishReasonOptions(translate)
    expect(options[0].value).toBeNull()
    expect(options[1].value).toBe(USAGE_FINISH_REASON_ABNORMAL)
    expect(options.map((o) => o.value)).toContain('content_filter')
  })
})
