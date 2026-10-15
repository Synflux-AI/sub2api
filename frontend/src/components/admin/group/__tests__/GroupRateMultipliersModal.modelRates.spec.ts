import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import type { AdminGroup } from '@/types'
import GroupRateMultipliersModal from '../GroupRateMultipliersModal.vue'

const mocks = vi.hoisted(() => ({
  getGroupRateMultipliers: vi.fn(),
  batchSetGroupRateMultipliers: vi.fn(),
  setUserGroupModelRateMultipliers: vi.fn(),
  showError: vi.fn(),
}))
vi.mock('@/api/admin', () => ({ adminAPI: { groups: mocks } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: mocks.showError, showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
beforeEach(() => vi.resetAllMocks())

const group = {
  id: 1, name: 'Group 1', platform: 'anthropic', rate_multiplier: 0.8,
  model_rate_multipliers: [
    { models: ['model-small'], rate_multiplier: 0.6 },
    { models: ['model-large-*'], rate_multiplier: 1 },
  ],
} as AdminGroup

const baseUser = { user_name: '', user_notes: '', user_status: 'active', rpm_override: null }

async function open(entries: unknown[]) {
  mocks.getGroupRateMultipliers.mockResolvedValue(entries)
  const wrapper = mount(GroupRateMultipliersModal, {
    props: { show: false, group },
    global: { stubs: {
      BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' },
      Icon: true, PlatformIcon: true, Pagination: true,
    } },
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

const save = (wrapper: Awaited<ReturnType<typeof open>>) =>
  wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')

describe('group rate multipliers modal: per-model overrides', () => {
  it('lists users that only have model overrides and shows inherited rows', async () => {
    const wrapper = await open([
      { ...baseUser, user_id: 2, user_email: 'only-model@example.com', rate_multiplier: null, model_rate_multipliers: { 'model-small': 0.5, removed: 0.1 } },
      { ...baseUser, user_id: 3, user_email: 'rate-only@example.com', rate_multiplier: null, model_rate_multipliers: null },
    ])
    const rows = wrapper.findAll('[data-testid="user-rate-row"]')
    expect(rows).toHaveLength(1)
    expect(rows[0].text()).toContain('only-model@example.com')
    expect(rows[0].text()).toContain('admin.groups.modelRates.overrideCount')
    // 残留在分组已删除规则上的修改不算未保存改动
    expect(wrapper.findAll('button').some(b => b.text() === 'common.save')).toBe(false)

    await wrapper.get('[data-testid="user-model-rate-toggle"]').trigger('click')
    const items = wrapper.findAll('[data-testid="user-model-rate-item"]')
    expect(items.map(item => item.text())).toEqual([
      expect.stringContaining('model-small'),
      expect.stringContaining('model-large-*'),
    ])
    expect((items[0].get('input').element as HTMLInputElement).value).toBe('0.5')
    expect((items[1].get('input').element as HTMLInputElement).value).toBe('')
  })

  it('saves changed model overrides per user without touching unchanged users', async () => {
    const wrapper = await open([
      { ...baseUser, user_id: 2, user_email: 'a@example.com', rate_multiplier: 0.5, model_rate_multipliers: {} },
      { ...baseUser, user_id: 4, user_email: 'b@example.com', rate_multiplier: 0.7, model_rate_multipliers: { 'model-small': 0.4 } },
    ])
    const toggles = wrapper.findAll('[data-testid="user-model-rate-toggle"]')
    await toggles[0].trigger('click')

    // 继承值 0.6 / 1.0 都高于用户专属倍率 0.5：两行都给出提示
    const itemWarning = (i: number) =>
      wrapper.findAll('[data-testid="user-model-rate-item"]')[i].find('[data-testid="user-model-rate-warning"]').exists()
    expect(itemWarning(0)).toBe(true)
    expect(itemWarning(1)).toBe(true)

    const input = wrapper.findAll('[data-testid="user-model-rate-input"]')[0]
    await input.setValue('0.5')
    await input.trigger('change')
    expect(itemWarning(0)).toBe(false)
    expect(itemWarning(1)).toBe(true)

    await save(wrapper)
    await flushPromises()
    expect(mocks.batchSetGroupRateMultipliers).toHaveBeenCalledWith(1, [
      { user_id: 2, rate_multiplier: 0.5 },
      { user_id: 4, rate_multiplier: 0.7 },
    ])
    expect(mocks.setUserGroupModelRateMultipliers).toHaveBeenCalledTimes(1)
    expect(mocks.setUserGroupModelRateMultipliers).toHaveBeenCalledWith(1, 2, { 'model-small': 0.5 })
  })

  it('resets to inherit and clears overrides of removed users', async () => {
    const wrapper = await open([
      { ...baseUser, user_id: 2, user_email: 'a@example.com', rate_multiplier: 0.5, model_rate_multipliers: { 'model-small': 0.45 } },
      { ...baseUser, user_id: 4, user_email: 'b@example.com', rate_multiplier: 0.7, model_rate_multipliers: { 'model-large-*': 0.9 } },
    ])
    await wrapper.findAll('[data-testid="user-model-rate-toggle"]')[0].trigger('click')
    await wrapper.get('[data-testid="user-model-rate-reset"]').trigger('click')

    // 删除第二个用户：其模型倍率修改也要清掉
    const removeButtons = wrapper.findAll('[data-testid="user-rate-row"]')[1].findAll('button')
    await removeButtons[removeButtons.length - 1].trigger('click')

    await save(wrapper)
    await flushPromises()
    expect(mocks.batchSetGroupRateMultipliers).toHaveBeenCalledWith(1, [{ user_id: 2, rate_multiplier: 0.5 }])
    expect(mocks.setUserGroupModelRateMultipliers.mock.calls).toEqual([
      [1, 2, {}],
      [1, 4, {}],
    ])
  })
})
