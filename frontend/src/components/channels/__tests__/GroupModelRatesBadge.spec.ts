import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import GroupModelRatesBadge from '../GroupModelRatesBadge.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key) })
}))

describe('GroupModelRatesBadge', () => {
  it('renders nothing without rules', () => {
    expect(mount(GroupModelRatesBadge, { props: { rules: [] }, global: { stubs: { Icon: true } } }).html()).toBe('<!--v-if-->')
  })

  it('lists the effective rate per pattern, preferring the user override', () => {
    const wrapper = mount(GroupModelRatesBadge, {
      props: {
        rules: [{ models: ['model-a', 'model-b*'], rate_multiplier: 0.6 }],
        userModelRates: { 'model-b*': 0.4 }
      },
      global: { stubs: { Icon: true } }
    })
    const badge = wrapper.get('[data-testid="group-model-rates-badge"]')
    expect(badge.text()).toContain('"count":2')
    expect(badge.attributes('title')).toContain('model-a: 0.6x')
    expect(badge.attributes('title')).toContain('model-b*: 0.4x')
  })
})
