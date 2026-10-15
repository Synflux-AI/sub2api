import { describe, expect, it } from 'vitest'

import {
  matchModelRateMultiplier,
  modelRatePatterns,
  normalizeModelRateName,
  resolveModelRate,
} from '../modelRateMultipliers'

const rules = [
  { models: ['example-family-*'], rate_multiplier: 1 },
  { models: ['example-family-small', 'Example-Other'], rate_multiplier: 0.6 },
]

describe('matchModelRateMultiplier', () => {
  it('prefers exact matches over an earlier wildcard', () => {
    expect(matchModelRateMultiplier(rules, 'example-family-small')).toEqual({ pattern: 'example-family-small', rate: 0.6 })
    expect(matchModelRateMultiplier(rules, 'example-family-large')).toEqual({ pattern: 'example-family-*', rate: 1 })
  })

  it('normalizes case and claude dots like the backend', () => {
    expect(matchModelRateMultiplier(rules, 'EXAMPLE-OTHER')?.pattern).toBe('Example-Other')
    expect(normalizeModelRateName(' claude-example-4.5 ')).toBe('claude-example-4-5')
    expect(matchModelRateMultiplier([{ models: ['claude-example-4.5'], rate_multiplier: 0.5 }], 'claude-example-4-5')?.rate).toBe(0.5)
  })

  it('returns null without rules or model', () => {
    expect(matchModelRateMultiplier(rules, 'unrelated')).toBeNull()
    expect(matchModelRateMultiplier(undefined, 'example-family-small')).toBeNull()
    expect(matchModelRateMultiplier(rules, '')).toBeNull()
  })
})

describe('resolveModelRate', () => {
  it('follows the backend precedence', () => {
    expect(resolveModelRate(0.8, rules, 'unrelated')).toEqual({ rate: 0.8, source: 'group', pattern: null })
    expect(resolveModelRate(0.8, rules, 'unrelated', 0.5)).toEqual({ rate: 0.5, source: 'user', pattern: null })
    expect(resolveModelRate(0.8, rules, 'example-family-small', 0.5)).toEqual({ rate: 0.6, source: 'group_model', pattern: 'example-family-small' })
    expect(resolveModelRate(0.8, rules, 'example-family-small', 0.5, { 'example-family-small': 0.45 })).toEqual({ rate: 0.45, source: 'user_model', pattern: 'example-family-small' })
    // 用户在其他模式串上的修改不影响命中精确规则的模型。
    expect(resolveModelRate(0.8, rules, 'example-family-small', 0.5, { 'example-family-*': 0.9 }).rate).toBe(0.6)
  })

  it('lists patterns in rule order', () => {
    expect(modelRatePatterns(rules)).toEqual(['example-family-*', 'example-family-small', 'Example-Other'])
  })
})
