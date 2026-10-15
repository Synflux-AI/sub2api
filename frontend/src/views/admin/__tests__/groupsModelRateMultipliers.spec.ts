import { describe, expect, it } from 'vitest'

import { modelRatesFromAPI, modelRatesToAPI, validateModelRateForm } from '../groupsModelRateMultipliers'

describe('groups model rate multipliers form', () => {
  it('round-trips API rules and drops empty cards', () => {
    const form = modelRatesFromAPI([{ models: ['model-a', 'model-b*'], rate_multiplier: 0.6 }])
    expect(form).toEqual([{ models: ['model-a', 'model-b*'], rate_multiplier: 0.6 }])
    form.push({ models: [], rate_multiplier: 2 })
    form[0].rate_multiplier = '0.55'
    expect(modelRatesToAPI(form)).toEqual([{ models: ['model-a', 'model-b*'], rate_multiplier: 0.55 }])
    expect(modelRatesFromAPI(undefined)).toEqual([])
  })

  it('validates like the backend', () => {
    expect(validateModelRateForm([{ models: ['m'], rate_multiplier: 0.5 }])).toBeNull()
    expect(validateModelRateForm([{ models: [], rate_multiplier: null }])).toBeNull()
    expect(validateModelRateForm([{ models: ['m'], rate_multiplier: 0 }])?.key).toBe('errorRate')
    expect(validateModelRateForm([{ models: ['m'], rate_multiplier: '' }])?.key).toBe('errorRate')
    expect(validateModelRateForm([{ models: ['*'], rate_multiplier: 1 }])?.key).toBe('errorBareWildcard')
    expect(validateModelRateForm([{ models: ['a*b'], rate_multiplier: 1 }])?.key).toBe('errorInnerWildcard')
    expect(validateModelRateForm([
      { models: ['Model-A'], rate_multiplier: 1 },
      { models: ['model-a'], rate_multiplier: 2 },
    ])).toEqual({ key: 'errorDuplicate', model: 'model-a' })
  })
})
