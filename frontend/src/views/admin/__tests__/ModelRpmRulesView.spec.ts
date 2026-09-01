import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

import ModelRpmRulesView from '../ModelRpmRulesView.vue'
import type { ModelRPMRule } from '@/types'

const { listRules, updateRule, deleteRule, listGroups, showError, showSuccess } = vi.hoisted(() => ({
  listRules: vi.fn(),
  updateRule: vi.fn(),
  deleteRule: vi.fn(),
  listGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    modelRpmRules: { list: listRules, update: updateRule, delete: deleteRule },
    groups: { list: listGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess, showWarning: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/utils/apiError', () => ({ extractApiErrorMessage: () => 'error' }))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string>) =>
        key.replace(/\{(\w+)\}/g, (_, token) => params?.[token] ?? `{${token}}`),
      locale: { value: 'zh-CN' }
    })
  }
})

const AppLayoutStub = { template: '<div><slot /></div>' }
const TablePageLayoutStub = {
  template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
}

const ToggleStub = defineComponent({
  name: 'Toggle',
  props: { modelValue: { type: Boolean, default: false } },
  emits: ['update:modelValue'],
  inheritAttrs: false,
  setup(props, { attrs, emit }) {
    return () =>
      h('input', {
        ...attrs,
        class: 'toggle-stub',
        type: 'checkbox',
        checked: props.modelValue,
        onChange: (event: Event) => emit('update:modelValue', (event.target as HTMLInputElement).checked)
      })
  }
})

const ConfirmDialogStub = defineComponent({
  name: 'ConfirmDialog',
  props: { show: { type: Boolean, default: false } },
  emits: ['confirm', 'cancel'],
  template:
    '<div v-if="show" data-testid="delete-dialog">' +
    '<button data-testid="delete-confirm" @click="$emit(\'confirm\')"></button>' +
    '</div>'
})

const rules: ModelRPMRule[] = [
  {
    id: 1,
    name: 'opus 全站池',
    model_pattern: 'claude-opus-*',
    scope: 'global',
    target_type: 'all',
    target_id: null,
    rpm_limit: 10,
    enabled: true,
    created_at: '2026-09-01T00:00:00Z',
    updated_at: '2026-09-01T00:00:00Z'
  },
  {
    id: 2,
    name: 'vip 分组',
    model_pattern: 'gpt-5',
    scope: 'user',
    target_type: 'group',
    target_id: 7,
    rpm_limit: 30,
    enabled: false,
    created_at: '2026-09-01T00:00:00Z',
    updated_at: '2026-09-01T00:00:00Z',
    target_name: 'vip'
  }
]

function mountView() {
  return mount(ModelRpmRulesView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        Toggle: ToggleStub,
        ConfirmDialog: ConfirmDialogStub,
        ModelRpmRuleModal: true,
        EmptyState: true,
        Icon: true
      }
    }
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  listRules.mockResolvedValue(rules.map((rule) => ({ ...rule })))
  listGroups.mockResolvedValue({ items: [{ id: 7, name: 'vip' }] })
  updateRule.mockResolvedValue({})
  deleteRule.mockResolvedValue({ message: 'ok' })
})

describe('ModelRpmRulesView', () => {
  it('列出规则并区分 scope 与适用范围', async () => {
    const wrapper = mountView()
    await flushPromises()

    const rows = wrapper.findAll('tbody tr[data-row-id]')
    expect(rows).toHaveLength(2)

    expect(rows[0].text()).toContain('claude-opus-*')
    expect(rows[0].text()).toContain('admin.modelRpmRules.scopes.global')
    expect(rows[0].text()).toContain('admin.modelRpmRules.targetTypes.all')

    // 分组规则展示 JOIN 出来的分组名，而不是裸 ID。
    expect(rows[1].text()).toContain('admin.modelRpmRules.targetTypes.group')
    expect(rows[1].text()).toContain('vip')
  })

  it('切换启用开关提交完整规则并保留 target_id', async () => {
    const wrapper = mountView()
    await flushPromises()

    const rows = wrapper.findAll('tbody tr[data-row-id]')
    await rows[1].get('.toggle-stub').setValue(true)
    await flushPromises()

    expect(updateRule).toHaveBeenCalledWith(2, {
      name: 'vip 分组',
      model_pattern: 'gpt-5',
      scope: 'user',
      target_type: 'group',
      target_id: 7,
      rpm_limit: 30,
      enabled: true
    })
  })

  it('target_type=all 的规则提交时不带 target_id', async () => {
    const wrapper = mountView()
    await flushPromises()

    const rows = wrapper.findAll('tbody tr[data-row-id]')
    await rows[0].get('.toggle-stub').setValue(false)
    await flushPromises()

    expect(updateRule).toHaveBeenCalledWith(1, expect.objectContaining({ target_type: 'all', target_id: null }))
  })

  it('删除需要二次确认后才调接口', async () => {
    const wrapper = mountView()
    await flushPromises()

    const rows = wrapper.findAll('tbody tr[data-row-id]')
    await rows[0].findAll('button')[1].trigger('click')
    expect(deleteRule).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="delete-confirm"]').trigger('click')
    await flushPromises()
    expect(deleteRule).toHaveBeenCalledWith(1)
    expect(showSuccess).toHaveBeenCalled()
  })

  it('加载失败时提示而不是静默空列表', async () => {
    listRules.mockRejectedValueOnce(new Error('boom'))
    mountView()
    await flushPromises()
    expect(showError).toHaveBeenCalled()
  })
})
