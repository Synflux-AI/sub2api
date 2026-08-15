/**
 * 账号调度状态判定。
 *
 * 硬闸门（gate）与软信号（健康分）是两类不同性质的约束：
 * - 硬闸门命中 → 账号被移出候选集，直到闸门解除；
 * - 健康分只改变账号在候选集内的排序，永不缩小候选集。
 *
 * 本文件只处理硬闸门。判定逻辑与后端 `ops_account_availability.go`
 * 的 GetAccountAvailabilityStats 保持一致（含互斥标志归一化），
 * 避免同一账号在运维大盘与智能调度页显示出互相矛盾的状态。
 */

import type { Account } from '@/types'

/** 硬闸门类型。`available` 表示未被任何闸门拦截。 */
export type SchedulingGate =
  | 'available'
  | 'inactive'
  | 'manual'
  | 'rate_limited'
  | 'overloaded'
  | 'temp_unschedulable'

export interface SchedulingGateState {
  /** 当前生效的闸门；多个同时命中时按 gates 的优先序取第一个。 */
  gate: SchedulingGate
  /** 所有命中的闸门（归一化后），供需要展示全部徽章的场景使用。 */
  gates: SchedulingGate[]
  /** 是否可调度。 */
  available: boolean
  /** 闸门自动解除的时间；null 表示需要人工处理（如手动停用、状态异常）。 */
  recoversAt: string | null
}

function isFuture(value: string | null | undefined, now: number): boolean {
  if (!value) return false
  const ts = new Date(value).getTime()
  return !Number.isNaN(ts) && ts > now
}

/**
 * 计算账号当前的硬闸门状态。
 *
 * 与后端一致的归一化：账号 status 为 error 时，429 / 529 标志不再单独展示 ——
 * 否则 UI 会同时亮起「状态异常」和「限流中」两个互相矛盾的徽章。
 */
export function resolveSchedulingGate(
  account: Account,
  nowMs: number = Date.now()
): SchedulingGateState {
  const hasError = account.status === 'error'

  let rateLimited = isFuture(account.rate_limit_reset_at, nowMs)
  let overloaded = isFuture(account.overload_until, nowMs)
  const tempUnschedulable = isFuture(account.temp_unschedulable_until, nowMs)

  // 归一化互斥标志，与后端保持一致。
  if (hasError) {
    rateLimited = false
    overloaded = false
  }

  const gates: SchedulingGate[] = []
  if (account.status !== 'active') gates.push('inactive')
  if (!account.schedulable) gates.push('manual')
  if (rateLimited) gates.push('rate_limited')
  if (overloaded) gates.push('overloaded')
  if (tempUnschedulable) gates.push('temp_unschedulable')

  const available =
    account.status === 'active' &&
    account.schedulable &&
    !rateLimited &&
    !overloaded &&
    !tempUnschedulable

  // 恢复时间取所有时间型闸门里最晚的一个：任一闸门未解除，账号就还不可调度。
  let recoversAt: string | null = null
  if (!available) {
    const candidates = [
      rateLimited ? account.rate_limit_reset_at : null,
      overloaded ? account.overload_until : null,
      tempUnschedulable ? account.temp_unschedulable_until : null
    ].filter((v): v is string => !!v)

    // 存在需要人工处理的闸门时不给恢复时间，避免「等一会就好了」的误导。
    const needsManualAction = account.status !== 'active' || !account.schedulable
    if (!needsManualAction && candidates.length > 0) {
      recoversAt = candidates.reduce((latest, cur) =>
        new Date(cur).getTime() > new Date(latest).getTime() ? cur : latest
      )
    }
  }

  return {
    gate: available ? 'available' : gates[0] ?? 'inactive',
    gates,
    available,
    recoversAt
  }
}

/** i18n key 后缀，配合 `admin.smartRouting.overview.reason*` 使用。 */
export const GATE_REASON_KEY: Record<Exclude<SchedulingGate, 'available'>, string> = {
  inactive: 'reasonInactive',
  manual: 'reasonManual',
  rate_limited: 'reasonRateLimit',
  overloaded: 'reasonOverload',
  temp_unschedulable: 'reasonTempUnsched'
}

/** 徽章配色。与账号管理页的既有语义保持一致：红=需人工，橙=限流，黄=过载/临时。 */
export const GATE_BADGE_CLASS: Record<SchedulingGate, string> = {
  available: 'badge-success',
  inactive: 'badge-danger',
  manual: 'badge-gray',
  rate_limited: 'badge-warning',
  overloaded: 'badge-warning',
  temp_unschedulable: 'badge-warning'
}

/** 健康分层 → i18n key 后缀，配合 `admin.smartRouting.accounts.healthTier*` 使用。 */
export function healthTierKey(tier: number | null | undefined): string {
  switch (tier) {
    case 1:
      return 'healthTier1'
    case 2:
      return 'healthTier2'
    default:
      return 'healthTier0'
  }
}

/** 健康分配色：主池绿、候选池黄、隔离观察红。 */
export function healthTierBadgeClass(tier: number | null | undefined): string {
  switch (tier) {
    case 1:
      return 'badge-warning'
    case 2:
      return 'badge-danger'
    default:
      return 'badge-success'
  }
}
