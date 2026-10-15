// 分组逐模型售价倍率表单辅助：API <-> 表单换算与提交前校验。
// 校验与后端 normalizeGroupModelRateMultipliers 同一口径，提前给出可读错误。

import type { GroupModelRateMultiplier } from '@/types'
import { normalizeModelRateName } from '@/utils/modelRateMultipliers'

export type ModelRateFormEntry = {
  models: string[]
  rate_multiplier: number | string | null
}

export const emptyModelRateEntry = (): ModelRateFormEntry => ({ models: [], rate_multiplier: null })

export const modelRatesFromAPI = (
  rules: GroupModelRateMultiplier[] | null | undefined,
): ModelRateFormEntry[] =>
  (rules || []).map((rule) => ({ models: [...(rule.models || [])], rate_multiplier: rule.rate_multiplier }))

// 未填模型的空卡片直接丢弃（与 model_pricing 表单一致）；其余条目原样交给后端，
// 非法值已由 validateModelRateForm 在提交前拦下。
export const modelRatesToAPI = (entries: ModelRateFormEntry[]): GroupModelRateMultiplier[] =>
  entries
    .map((entry) => ({
      models: entry.models.map((m) => m.trim()).filter(Boolean),
      rate_multiplier: Number(entry.rate_multiplier),
    }))
    .filter((entry) => entry.models.length > 0)

export type ModelRateFormError = {
  /** 相对 admin.groups.modelRates 前缀的 i18n key */
  key: 'errorRate' | 'errorBareWildcard' | 'errorInnerWildcard' | 'errorDuplicate'
  model?: string
}

export const validateModelRateForm = (entries: ModelRateFormEntry[]): ModelRateFormError | null => {
  const seen = new Set<string>()
  for (const entry of entries) {
    const models = entry.models.map((m) => m.trim()).filter(Boolean)
    if (models.length === 0) {
      continue
    }
    const rate = Number(entry.rate_multiplier)
    if (entry.rate_multiplier === null || entry.rate_multiplier === '' || !Number.isFinite(rate) || rate <= 0) {
      return { key: 'errorRate', model: models.join(', ') }
    }
    for (const model of models) {
      if (model === '*') {
        return { key: 'errorBareWildcard', model }
      }
      const star = model.indexOf('*')
      if (star >= 0 && star !== model.length - 1) {
        return { key: 'errorInnerWildcard', model }
      }
      const normalized = normalizeModelRateName(model)
      if (seen.has(normalized)) {
        return { key: 'errorDuplicate', model }
      }
      seen.add(normalized)
    }
  }
  return null
}
