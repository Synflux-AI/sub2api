import { describe, expect, it } from 'vitest'

import type { Account } from '@/types'
import { resolveSchedulingGate, resolveScopedRateLimits } from '../schedulingState'

const NOW = Date.UTC(2026, 7, 15, 12, 0, 0)

function iso(offsetMs: number): string {
  return new Date(NOW + offsetMs).toISOString()
}

/** 只填闸门判定用得到的字段；其余用 as 断言绕过，避免测试跟着 Account 的无关字段漂移。 */
function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: 1,
    name: 'acc',
    platform: 'anthropic',
    status: 'active',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    ...overrides
  } as Account
}

describe('resolveSchedulingGate', () => {
  it('treats an active, schedulable account with no cooldown as available', () => {
    const state = resolveSchedulingGate(makeAccount(), NOW)

    expect(state.available).toBe(true)
    expect(state.gate).toBe('available')
    expect(state.gates).toEqual([])
    expect(state.recoversAt).toBeNull()
  })

  it('ignores cooldown timestamps that are already in the past', () => {
    const state = resolveSchedulingGate(
      makeAccount({
        rate_limit_reset_at: iso(-60_000),
        overload_until: iso(-60_000),
        temp_unschedulable_until: iso(-60_000)
      }),
      NOW
    )

    expect(state.available).toBe(true)
    expect(state.gates).toEqual([])
  })

  it('reports a future 429 reset as rate limited and exposes the recovery time', () => {
    const resetAt = iso(10 * 60_000)
    const state = resolveSchedulingGate(makeAccount({ rate_limit_reset_at: resetAt }), NOW)

    expect(state.available).toBe(false)
    expect(state.gates).toEqual(['rate_limited'])
    expect(state.recoversAt).toBe(resetAt)
  })

  it('takes the latest timestamp when several time-based gates are active', () => {
    const later = iso(30 * 60_000)
    const state = resolveSchedulingGate(
      makeAccount({
        rate_limit_reset_at: iso(5 * 60_000),
        overload_until: later,
        temp_unschedulable_until: iso(10 * 60_000)
      }),
      NOW
    )

    // 任一闸门未解除账号就仍不可调度，因此恢复时间取最晚的那个。
    expect(state.recoversAt).toBe(later)
    expect(state.gates).toEqual(['rate_limited', 'overloaded', 'temp_unschedulable'])
  })

  it('suppresses 429/529 badges when the account status is error, mirroring the backend', () => {
    const state = resolveSchedulingGate(
      makeAccount({
        status: 'error',
        rate_limit_reset_at: iso(10 * 60_000),
        overload_until: iso(10 * 60_000)
      }),
      NOW
    )

    // 后端 GetAccountAvailabilityStats 会归一化互斥标志，避免同时亮起
    // 「状态异常」和「限流中」两个互相矛盾的徽章。
    expect(state.gates).toEqual(['inactive'])
    expect(state.available).toBe(false)
  })

  it('does not offer a recovery time when the account needs manual action', () => {
    const manual = resolveSchedulingGate(
      makeAccount({ schedulable: false, rate_limit_reset_at: iso(10 * 60_000) }),
      NOW
    )
    expect(manual.gates).toEqual(['manual', 'rate_limited'])
    // 手动停用不会随时间自愈，给出倒计时会误导运维以为「等一会就好了」。
    expect(manual.recoversAt).toBeNull()

    const inactive = resolveSchedulingGate(makeAccount({ status: 'inactive' }), NOW)
    expect(inactive.gates).toEqual(['inactive'])
    expect(inactive.recoversAt).toBeNull()
  })

  it('keeps a manually disabled account unavailable even when nothing else is wrong', () => {
    const state = resolveSchedulingGate(makeAccount({ schedulable: false }), NOW)

    expect(state.available).toBe(false)
    expect(state.gate).toBe('manual')
  })

  it('exposes active model-scoped 429 cooldowns without blocking the whole account', () => {
    const state = resolveSchedulingGate(
      makeAccount({
        extra: {
          model_rate_limits: {
            opus: {
              rate_limited_at: iso(-60_000),
              rate_limit_reset_at: iso(10 * 60_000)
            },
            expired: {
              rate_limited_at: iso(-120_000),
              rate_limit_reset_at: iso(-60_000)
            }
          }
        }
      }),
      NOW
    )

    expect(state.available).toBe(true)
    expect(state.gates).toEqual([])
    expect(state.scopedRateLimits).toEqual([
      { scope: 'opus', recoversAt: iso(10 * 60_000) }
    ])
  })

  it('sorts model-scoped cooldowns by recovery time', () => {
    const account = makeAccount({
      extra: {
        model_rate_limits: {
          later: {
            rate_limited_at: iso(-60_000),
            rate_limit_reset_at: iso(20 * 60_000)
          },
          sooner: {
            rate_limited_at: iso(-60_000),
            rate_limit_reset_at: iso(5 * 60_000)
          }
        }
      }
    })

    expect(resolveScopedRateLimits(account, NOW).map((item) => item.scope)).toEqual([
      'sooner',
      'later'
    ])
  })
})
