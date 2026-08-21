import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'

import RoutingStrategiesPanel from '../RoutingStrategiesPanel.vue'
import Select from '@/components/common/Select.vue'
import MultiSelect from '@/components/common/MultiSelect.vue'
import type { AdminGroup, Account, RoutingStrategy } from '@/types'

const { listStrategies, createStrategy, updateStrategy, getAllGroups, listAccounts, showSuccess, showError } =
  vi.hoisted(() => ({
    listStrategies: vi.fn(),
    createStrategy: vi.fn(),
    updateStrategy: vi.fn(),
    getAllGroups: vi.fn(),
    listAccounts: vi.fn(),
    showSuccess: vi.fn(),
    showError: vi.fn()
  }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    routingStrategies: {
      list: listStrategies,
      create: createStrategy,
      update: updateStrategy,
      delete: vi.fn()
    },
    groups: {
      getAll: getAllGroups
    },
    accounts: {
      list: listAccounts
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showSuccess,
    showError
  })
}))

// vitest.config.ts 把 vue-i18n 别名指向 runtime-only 构建，该构建不含消息编译器，
// 任何真实 createI18n() 消息树在这个测试环境下 t() 都只会原样返回 key（AccountTestModal.spec.ts /
// BulkEditUserModal.spec.ts 等既有用例都是这样处理的）。这里沿用同一套方案：mock useI18n()，
// 用产品真实文案（摘自 en 文案文件、本用例实际会断言到的几个 key）做一个可插值的 t()，
// 保证折叠文案等断言对照的是产品真实拷贝，而不是测试自造的占位符。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  const messages: Record<string, string> = {
    'common.edit': 'Edit',
    'common.delete': 'Delete',
    'common.refresh': 'Refresh',
    'common.actions': 'Actions',
    'admin.routingStrategies.globalScope': 'Global (all groups)',
    'admin.routingStrategies.groupsMore': '+{count}'
  }
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) => {
        const raw = messages[key] ?? key
        if (!params) return raw
        return raw.replace(/\{(\w+)\}/g, (_match, name) => String(params[name] ?? `{${name}}`))
      }
    })
  }
})

function makeGroup(overrides: Partial<AdminGroup> & { id: number; name: string; platform: string }): AdminGroup {
  return {
    description: null,
    rate_multiplier: 1,
    is_exclusive: false,
    status: 'active',
    subscription_type: 'none',
    daily_limit_usd: null,
    weekly_limit_usd: null,
    monthly_limit_usd: null,
    long_context_pricing_enabled: false,
    allow_image_generation: false,
    allow_batch_image_generation: false,
    image_rate_independent: false,
    image_rate_multiplier: 1,
    model_pricing: [],
    profit_control_enabled: false,
    profit_min_margin: 0,
    profit_safety_buffer: 0,
    model_routing: null,
    model_routing_enabled: false,
    mcp_xml_inject: false,
    sort_order: 0,
    ...overrides
  } as unknown as AdminGroup
}

function makeAccount(overrides: Partial<Account> & { id: number; name: string; platform: string }): Account {
  return {
    type: 'oauth',
    ...overrides
  } as unknown as Account
}

function makeStrategy(overrides: Partial<RoutingStrategy> & { id: number }): RoutingStrategy {
  return {
    name: 'strategy',
    description: '',
    enabled: true,
    priority: 100,
    platform: 'anthropic',
    group_ids: [],
    match_mode: 'all',
    conditions: [],
    action: 'restrict',
    account_ids: [],
    account_priorities: [],
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides
  } as RoutingStrategy
}

const stubs = {
  BaseDialog: {
    props: ['show', 'title', 'width'],
    emits: ['close'],
    template: '<div v-if="show"><slot /><slot name="footer" /></div>'
  },
  ConfirmDialog: {
    props: ['show', 'title', 'message', 'confirmText', 'cancelText', 'danger'],
    emits: ['confirm', 'cancel'],
    template: '<div />'
  },
  Icon: true
}

function mountPanel() {
  return mount(RoutingStrategiesPanel, {
    global: {
      stubs
    }
  })
}

// 定位分组多选控件：组件类型唯一，规避 DOM class 与其它 Select 实例撞名。
function groupMultiSelect(wrapper: ReturnType<typeof mountPanel>) {
  return wrapper.findComponent(MultiSelect)
}

// create/edit 表单里的 Select 实例按模板顺序排列：platform / action / match_mode（无条件时）。
function formSelects(wrapper: ReturnType<typeof mountPanel>) {
  return wrapper.findAllComponents(Select)
}

async function openMultiSelectDropdown(triggerWrapper: ReturnType<typeof mountPanel>) {
  await triggerWrapper.get('button.select-trigger').trigger('click')
  await nextTick()
  return document.body.querySelector('.select-dropdown-portal') as HTMLElement
}

function clickOptionByText(dropdown: HTMLElement, text: string, selector = 'button.select-option, div.select-option') {
  const option = [...dropdown.querySelectorAll<HTMLElement>(selector)].find((el) =>
    el.textContent?.trim().startsWith(text)
  )
  if (!option) throw new Error(`option "${text}" not found`)
  option.click()
}

describe('RoutingStrategiesPanel', () => {
  beforeEach(() => {
    listStrategies.mockReset()
    createStrategy.mockReset()
    updateStrategy.mockReset()
    getAllGroups.mockReset()
    listAccounts.mockReset()
    showSuccess.mockReset()
    showError.mockReset()

    listStrategies.mockResolvedValue([])
    createStrategy.mockResolvedValue({})
    updateStrategy.mockResolvedValue({})
    getAllGroups.mockResolvedValue([])
    listAccounts.mockResolvedValue({ items: [] })
  })

  afterEach(() => {
    document.body.innerHTML = ''
    vi.restoreAllMocks()
  })

  it('submits the create payload with group_ids from the MultiSelect selection', async () => {
    getAllGroups.mockResolvedValue([
      makeGroup({ id: 1, name: 'ccmax', platform: 'anthropic' }),
      makeGroup({ id: 2, name: 'codex', platform: 'openai' })
    ])
    listAccounts.mockResolvedValue({
      items: [makeAccount({ id: 10, name: 'acc-anthropic', platform: 'anthropic' })]
    })

    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('button.btn-primary').trigger('click') // "Create Strategy"
    await nextTick()

    await wrapper.get('input[type="text"]').setValue('my strategy')

    // Select the one target account (platform defaults to anthropic, matching acc-anthropic).
    const accountCheckbox = wrapper.get('input[type="checkbox"]')
    await accountCheckbox.setValue(true)

    // Pick "ccmax" (id 1, anthropic) from the group MultiSelect.
    const multiSelect = groupMultiSelect(wrapper)
    const dropdown = await openMultiSelectDropdown(multiSelect)
    clickOptionByText(dropdown, 'ccmax')
    await nextTick()

    await wrapper.get('form#routing-strategy-form').trigger('submit')
    await flushPromises()

    expect(createStrategy).toHaveBeenCalledTimes(1)
    const payload = createStrategy.mock.calls[0][0]
    expect(payload.group_ids).toEqual([1])
    expect(payload.name).toBe('my strategy')
  })

  it('backfills form.group_ids when editing an existing strategy and preserves them on save', async () => {
    getAllGroups.mockResolvedValue([
      makeGroup({ id: 1, name: 'ccmax', platform: 'openai' }),
      makeGroup({ id: 2, name: 'codex', platform: 'openai' })
    ])
    listAccounts.mockResolvedValue({
      items: [makeAccount({ id: 10, name: 'acc-openai', platform: 'openai' })]
    })
    listStrategies.mockResolvedValue([
      makeStrategy({
        id: 5,
        name: 'existing',
        platform: 'openai',
        group_ids: [2],
        account_ids: [10],
        account_priorities: [1]
      })
    ])

    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('[title="Edit"]').trigger('click')
    await nextTick()

    // The MultiSelect trigger should show the backfilled group name, not the "Global" default.
    const multiSelect = groupMultiSelect(wrapper)
    expect(multiSelect.get('button.select-trigger').text()).toContain('codex')

    await wrapper.get('form#routing-strategy-form').trigger('submit')
    await flushPromises()

    expect(updateStrategy).toHaveBeenCalledTimes(1)
    const [id, payload] = updateStrategy.mock.calls[0]
    expect(id).toBe(5)
    expect(payload.group_ids).toEqual([2])
  })

  it('drops selected groups that no longer match the platform when platform changes', async () => {
    getAllGroups.mockResolvedValue([
      makeGroup({ id: 1, name: 'AnthropicOnly', platform: 'anthropic' }),
      makeGroup({ id: 2, name: 'OpenAIOnly', platform: 'openai' }),
      makeGroup({ id: 3, name: 'CompositeGroup', platform: 'composite' })
    ])

    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('button.btn-primary').trigger('click') // opens create dialog, platform defaults to anthropic
    await nextTick()

    // Select the anthropic-only group.
    const multiSelect = groupMultiSelect(wrapper)
    const dropdown = await openMultiSelectDropdown(multiSelect)
    clickOptionByText(dropdown, 'AnthropicOnly')
    await nextTick()
    expect(multiSelect.get('button.select-trigger').text()).toContain('AnthropicOnly')

    // Switch platform to openai: groupOptions no longer contains id 1, so it must be dropped.
    const platformSelect = formSelects(wrapper)[0]
    const platformDropdown = await openMultiSelectDropdown(platformSelect)
    clickOptionByText(platformDropdown, 'openai', 'div.select-option')
    await nextTick()

    expect(multiSelect.get('button.select-trigger').text()).toContain('Global')
    expect(multiSelect.props('modelValue')).toEqual([])
  })

  it('keeps a dangling group id (not present in the loaded group list) when opening the edit dialog', async () => {
    // Group 99 was soft-deleted, so groups.getAll() no longer returns it. The design decision is
    // "dangling ids are not cleaned up, they simply stop matching" — silently dropping it here would
    // turn group_ids into [] and promote the (restrict) strategy to global scope.
    getAllGroups.mockResolvedValue([makeGroup({ id: 1, name: 'ccmax', platform: 'anthropic' })])
    listAccounts.mockResolvedValue({
      items: [makeAccount({ id: 10, name: 'acc-openai', platform: 'openai' })]
    })
    listStrategies.mockResolvedValue([
      makeStrategy({
        id: 7,
        name: 'dangling',
        platform: 'openai',
        group_ids: [99],
        account_ids: [10],
        account_priorities: [1]
      })
    ])

    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('[title="Edit"]').trigger('click')
    await nextTick()
    await nextTick()

    const multiSelect = groupMultiSelect(wrapper)
    expect(multiSelect.props('modelValue')).toEqual([99])
    expect(multiSelect.get('button.select-trigger').text()).toContain('#99')

    await wrapper.get('form#routing-strategy-form').trigger('submit')
    await flushPromises()

    expect(updateStrategy).toHaveBeenCalledTimes(1)
    expect(updateStrategy.mock.calls[0][1].group_ids).toEqual([99])
  })

  it('keeps the selected groups when the group list failed to load', async () => {
    // loadGroups() swallows the error and leaves groups.value = []; every id then looks "unknown",
    // and must therefore be preserved rather than filtered away.
    getAllGroups.mockRejectedValue(new Error('boom'))
    listAccounts.mockResolvedValue({
      items: [makeAccount({ id: 10, name: 'acc-openai', platform: 'openai' })]
    })
    listStrategies.mockResolvedValue([
      makeStrategy({
        id: 8,
        name: 'groups-unavailable',
        platform: 'openai',
        group_ids: [2, 3],
        account_ids: [10],
        account_priorities: [1]
      })
    ])

    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('[title="Edit"]').trigger('click')
    await nextTick()
    await nextTick()

    const multiSelect = groupMultiSelect(wrapper)
    expect(multiSelect.props('modelValue')).toEqual([2, 3])
    expect(multiSelect.get('button.select-trigger').text()).not.toContain('Global')

    await wrapper.get('form#routing-strategy-form').trigger('submit')
    await flushPromises()

    expect(updateStrategy).toHaveBeenCalledTimes(1)
    expect(updateStrategy.mock.calls[0][1].group_ids).toEqual([2, 3])
  })

  it('keeps a dangling group id when the user changes the platform', async () => {
    getAllGroups.mockResolvedValue([
      makeGroup({ id: 1, name: 'AnthropicOnly', platform: 'anthropic' }),
      makeGroup({ id: 2, name: 'OpenAIOnly', platform: 'openai' })
    ])
    listStrategies.mockResolvedValue([
      makeStrategy({ id: 9, name: 'dangling', platform: 'anthropic', group_ids: [1, 99] })
    ])

    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.get('[title="Edit"]').trigger('click')
    await nextTick()

    const multiSelect = groupMultiSelect(wrapper)
    expect(multiSelect.props('modelValue')).toEqual([1, 99])

    // Switch to openai: known-but-now-invalid id 1 is dropped, unknown id 99 survives.
    const platformSelect = formSelects(wrapper)[0]
    const platformDropdown = await openMultiSelectDropdown(platformSelect)
    clickOptionByText(platformDropdown, 'openai', 'div.select-option')
    await nextTick()

    expect(multiSelect.props('modelValue')).toEqual([99])
  })

  it('folds more than two scope groups into "name、name +N" with the full list in the title', async () => {
    getAllGroups.mockResolvedValue([
      makeGroup({ id: 1, name: 'ccmax', platform: 'anthropic' }),
      makeGroup({ id: 2, name: 'codex', platform: 'anthropic' }),
      makeGroup({ id: 3, name: 'gamma', platform: 'anthropic' }),
      makeGroup({ id: 4, name: 'delta', platform: 'anthropic' })
    ])
    listStrategies.mockResolvedValue([
      makeStrategy({ id: 1, name: 'global-strategy', group_ids: [] }),
      makeStrategy({ id: 2, name: 'scoped-strategy', group_ids: [1, 2, 3, 4] })
    ])

    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('Global (all groups)')
    expect(wrapper.text()).toContain('ccmax、codex +2')
    expect(wrapper.html()).toContain('title="ccmax、codex、gamma、delta"')
  })
})
