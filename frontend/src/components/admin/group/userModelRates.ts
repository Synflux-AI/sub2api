// 分组「专属倍率」弹窗里用户模型倍率的纯逻辑：展开继承行、判定修改、生成保存请求。
// 语义见后端 resolveBillingBaseRate：用户只能修改分组已有的模型规则；未修改的规则继承分组。

import type { GroupModelRateMultiplier } from '@/types'

export type UserModelRateOverrides = Record<string, number>

export interface UserModelRateRow {
  pattern: string
  /** 分组规则的倍率（继承时的生效值） */
  groupRate: number
  /** 用户改过的值；null 表示继承 */
  override: number | null
  effective: number
  /** 继承值高于该用户专属倍率：继承后这个模型反而比用户其他模型贵 */
  inheritedAboveUserRate: boolean
}

export function buildUserModelRateRows(
  rules: GroupModelRateMultiplier[] | null | undefined,
  userRate: number | null | undefined,
  overrides: UserModelRateOverrides | null | undefined,
): UserModelRateRow[] {
  const rows: UserModelRateRow[] = []
  for (const rule of rules || []) {
    for (const pattern of rule.models || []) {
      const value = overrides?.[pattern]
      const override = value === undefined || value === null ? null : value
      rows.push({
        pattern,
        groupRate: rule.rate_multiplier,
        override,
        effective: override ?? rule.rate_multiplier,
        inheritedAboveUserRate: override === null && userRate != null && rule.rate_multiplier > userRate,
      })
    }
  }
  return rows
}

/** 只保留分组现有规则模式串上的修改（分组删改规则后的残留在后端也会被清理）。 */
export function pruneUserModelRateOverrides(
  rules: GroupModelRateMultiplier[] | null | undefined,
  overrides: UserModelRateOverrides | null | undefined,
): UserModelRateOverrides {
  const patterns = new Set((rules || []).flatMap((rule) => rule.models || []))
  const out: UserModelRateOverrides = {}
  for (const [pattern, rate] of Object.entries(overrides || {})) {
    if (patterns.has(pattern)) {
      out[pattern] = rate
    }
  }
  return out
}

export function userModelRateOverridesEqual(
  a: UserModelRateOverrides | null | undefined,
  b: UserModelRateOverrides | null | undefined,
): boolean {
  const left = Object.entries(a || {})
  const right = b || {}
  if (left.length !== Object.keys(right).length) {
    return false
  }
  return left.every(([pattern, rate]) => right[pattern] === rate)
}

export interface UserModelRateSave {
  user_id: number
  model_rate_multipliers: UserModelRateOverrides
}

/**
 * 对比服务端与本地，返回需要整体替换模型倍率修改的用户。
 * 本地移除的用户若原本有修改，发送 `{}` 让其全部恢复继承。
 */
export function diffUserModelRateSaves(
  server: Array<{ user_id: number; model_rate_multipliers?: UserModelRateOverrides | null }>,
  local: Array<{ user_id: number; model_rate_multipliers?: UserModelRateOverrides | null }>,
): UserModelRateSave[] {
  const serverByUser = new Map(server.map((e) => [e.user_id, e.model_rate_multipliers || {}]))
  const saves: UserModelRateSave[] = []
  const seen = new Set<number>()
  for (const entry of local) {
    seen.add(entry.user_id)
    const next = entry.model_rate_multipliers || {}
    if (!userModelRateOverridesEqual(serverByUser.get(entry.user_id), next)) {
      saves.push({ user_id: entry.user_id, model_rate_multipliers: { ...next } })
    }
  }
  for (const [userId, previous] of serverByUser) {
    if (!seen.has(userId) && Object.keys(previous).length > 0) {
      saves.push({ user_id: userId, model_rate_multipliers: {} })
    }
  }
  return saves
}
