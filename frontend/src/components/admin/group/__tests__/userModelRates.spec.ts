import { describe, expect, it } from 'vitest'

import {
  buildUserModelRateRows,
  diffUserModelRateSaves,
  pruneUserModelRateOverrides,
  userModelRateOverridesEqual,
} from '../userModelRates'

const rules = [
  { models: ['model-small'], rate_multiplier: 0.6 },
  { models: ['model-large-*'], rate_multiplier: 1 },
]

describe('userModelRates', () => {
  it('expands group rules with inherit/override state and flags inherited rates above the user rate', () => {
    expect(buildUserModelRateRows(rules, 0.5, { 'model-large-*': 0.9 })).toEqual([
      { pattern: 'model-small', groupRate: 0.6, override: null, effective: 0.6, inheritedAboveUserRate: true },
      { pattern: 'model-large-*', groupRate: 1, override: 0.9, effective: 0.9, inheritedAboveUserRate: false },
    ])
    expect(buildUserModelRateRows(rules, null, {})[0].inheritedAboveUserRate).toBe(false)
    expect(buildUserModelRateRows(undefined, 0.5, {})).toEqual([])
  })

  it('prunes overrides on patterns the group no longer has', () => {
    expect(pruneUserModelRateOverrides(rules, { 'model-small': 0.5, removed: 0.1 })).toEqual({ 'model-small': 0.5 })
  })

  it('compares overrides by value', () => {
    expect(userModelRateOverridesEqual({ a: 1 }, { a: 1 })).toBe(true)
    expect(userModelRateOverridesEqual({ a: 1 }, { a: 2 })).toBe(false)
    expect(userModelRateOverridesEqual(undefined, {})).toBe(true)
    expect(userModelRateOverridesEqual({ a: 1 }, {})).toBe(false)
  })

  it('emits saves only for changed users and resets removed users', () => {
    const server = [
      { user_id: 1, model_rate_multipliers: { 'model-small': 0.5 } },
      { user_id: 2, model_rate_multipliers: null },
      { user_id: 3, model_rate_multipliers: { 'model-small': 0.4 } },
      { user_id: 4, model_rate_multipliers: null },
    ]
    const local = [
      { user_id: 1, model_rate_multipliers: { 'model-small': 0.5 } },
      { user_id: 2, model_rate_multipliers: { 'model-large-*': 0.8 } },
      { user_id: 5, model_rate_multipliers: {} },
    ]
    expect(diffUserModelRateSaves(server, local)).toEqual([
      { user_id: 2, model_rate_multipliers: { 'model-large-*': 0.8 } },
      { user_id: 3, model_rate_multipliers: {} },
    ])
  })
})
