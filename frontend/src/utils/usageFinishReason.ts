// 与后端 responseoutcome.Reason* / AbnormalReasons 保持一致。
export const USAGE_FINISH_REASONS = [
  'stop',
  'tool_use',
  'pause_turn',
  'truncated',
  'refusal',
  'content_filter',
  'incomplete',
  'failed',
  'cancelled',
  'interrupted',
  'empty',
  'unknown',
] as const

export type UsageFinishReason = (typeof USAGE_FINISH_REASONS)[number]

// 筛选参数中代表"异常结束"集合的取值；unknown 单独展示，不计入异常。
export const USAGE_FINISH_REASON_ABNORMAL = 'abnormal'

const ABNORMAL = new Set<string>([
  'truncated',
  'refusal',
  'content_filter',
  'incomplete',
  'failed',
  'cancelled',
  'interrupted',
  'empty',
])

export function isAbnormalUsageFinishReason(reason?: string | null): boolean {
  return !!reason && ABNORMAL.has(reason)
}

export function getUsageFinishReasonLabel(
  reason: string | null | undefined,
  translate: (key: string) => string,
): string {
  if (!reason) return translate('usage.finishReasonNotCollected')
  if ((USAGE_FINISH_REASONS as readonly string[]).includes(reason)) {
    return translate(`usage.finishReasons.${reason}`)
  }
  return reason
}

export function getUsageFinishReasonBadgeClass(reason?: string | null): string {
  if (!reason) return 'text-gray-400 dark:text-gray-500'
  if (reason === 'refusal' || reason === 'content_filter' || reason === 'failed') {
    return 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  }
  if (isAbnormalUsageFinishReason(reason)) {
    return 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  }
  if (reason === 'unknown') {
    return 'bg-gray-100 text-gray-600 dark:bg-gray-800 dark:text-gray-300'
  }
  return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
}

export function buildUsageFinishReasonOptions(translate: (key: string) => string) {
  return [
    { value: null, label: translate('usage.finishReasonAll') },
    { value: USAGE_FINISH_REASON_ABNORMAL, label: translate('usage.finishReasonAbnormal') },
    ...USAGE_FINISH_REASONS.map((reason) => ({ value: reason, label: translate(`usage.finishReasons.${reason}`) })),
  ]
}
