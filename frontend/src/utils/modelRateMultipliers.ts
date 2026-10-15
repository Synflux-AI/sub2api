// 分组逐模型售价倍率的前端匹配与解析，与后端 Group.MatchModelRateMultiplier /
// resolveBillingBaseRate 同一口径（backend/internal/service/group_model_rate.go）：
//   - 模型名归一化：去首尾空白、转小写；claude- 前缀的模型把 "." 换成 "-"；
//   - 先在全部规则里找精确匹配，再取第一个命中的 `前缀*` 通配；
//   - base 倍率（全部替换，不相乘）：用户改过的模型倍率 > 分组模型倍率 > 用户专属倍率 > 分组默认。

import type { GroupModelRateMultiplier } from '@/types'

export type ModelRateSource = 'user_model' | 'group_model' | 'user' | 'group'

export interface ModelRateMatch {
  /** 规则里原样保存的模式串；用户对规则的修改以它为键。 */
  pattern: string
  rate: number
}

export function normalizeModelRateName(model: string): string {
  let out = (model || '').trim().toLowerCase()
  if (out.startsWith('claude-')) {
    out = out.replace(/\./g, '-')
  }
  return out
}

export function matchModelRateMultiplier(
  rules: GroupModelRateMultiplier[] | null | undefined,
  model: string,
): ModelRateMatch | null {
  const target = normalizeModelRateName(model)
  if (!target || !rules?.length) {
    return null
  }
  let wildcard: ModelRateMatch | null = null
  for (const rule of rules) {
    for (const pattern of rule.models || []) {
      const normalized = normalizeModelRateName(pattern)
      if (normalized === target) {
        return { pattern, rate: rule.rate_multiplier }
      }
      if (!wildcard && normalized.endsWith('*') && target.startsWith(normalized.slice(0, -1))) {
        wildcard = { pattern, rate: rule.rate_multiplier }
      }
    }
  }
  return wildcard
}

export interface ResolvedModelRate {
  rate: number
  source: ModelRateSource
  pattern: string | null
}

/** 按五层顺序（不含系统默认）解析某用户在分组下某模型的 base 倍率。 */
export function resolveModelRate(
  groupRate: number,
  rules: GroupModelRateMultiplier[] | null | undefined,
  model: string,
  userRate?: number | null,
  userModelRates?: Record<string, number> | null,
): ResolvedModelRate {
  const match = matchModelRateMultiplier(rules, model)
  if (match) {
    const override = userModelRates?.[match.pattern]
    if (override !== undefined && override !== null) {
      return { rate: override, source: 'user_model', pattern: match.pattern }
    }
    return { rate: match.rate, source: 'group_model', pattern: match.pattern }
  }
  if (userRate !== undefined && userRate !== null) {
    return { rate: userRate, source: 'user', pattern: null }
  }
  return { rate: groupRate, source: 'group', pattern: null }
}

/** 分组全部规则的模式串（原样，按规则顺序）。 */
export function modelRatePatterns(rules: GroupModelRateMultiplier[] | null | undefined): string[] {
  return (rules || []).flatMap((rule) => rule.models || [])
}
