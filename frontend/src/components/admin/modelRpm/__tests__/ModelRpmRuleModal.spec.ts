import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'

import ModelRpmRuleModal from '../ModelRpmRuleModal.vue'
import type { Group, ModelRPMRule } from '@/types'

const { createRule, updateRule, listUsers, showError, showSuccess } = vi.hoisted(() => ({
  createRule: vi.fn(),
  updateRule: vi.fn(),
  listUsers: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    modelRpmRules: { create: createRule, update: updateRule },
    users: { list: listUsers }
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
    useI18n: () => ({ t: (key: string) => key, locale: { value: 'zh-CN' } })
  }
})

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show" data-testid="rule-dialog"><slot /><slot name="footer" /></div>'
})

const ToggleStub = defineComponent({
  name: 'Toggle',
  props: { modelValue: { type: Boolean, default: false } },
  emits: ['update:modelValue'],
  template: '<input class="toggle-stub" type="checkbox" :checked="modelValue" />'
})

const groups: Group[] = [{ id: 7, name: 'vip' } as Group]

function mountModal(rule: ModelRPMRule | null = null) {
  return mount(ModelRpmRuleModal, {
    props: { show: true, rule, groups },
    global: { stubs: { BaseDialog: BaseDialogStub, Toggle: ToggleStub, Icon: true } }
  })
}

function saveButton(wrapper: ReturnType<typeof mountModal>) {
  return wrapper.findAll('button').find((btn) => btn.text() === 'common.save')!
}

beforeEach(() => {
  vi.clearAllMocks()
  createRule.mockResolvedValue({})
  updateRule.mockResolvedValue({})
  listUsers.mockResolvedValue({ items: [{ id: 42, username: 'alice', email: 'alice@example.com' }] })
})

describe('ModelRpmRuleModal', () => {
  it('新建规则提交归一化后的载荷', async () => {
    const wrapper = mountModal()
    await wrapper.get('#model-rpm-rule-name').setValue('  opus  ')
    await wrapper.get('#model-rpm-rule-pattern').setValue('  Claude-Opus-*  ')
    await wrapper.get('#model-rpm-rule-scope').setValue('global')
    await wrapper.get('#model-rpm-rule-limit').setValue('25')

    await saveButton(wrapper).trigger('click')
    await flushPromises()

    expect(createRule).toHaveBeenCalledWith({
      name: 'opus',
      model_pattern: 'Claude-Opus-*',
      scope: 'global',
      target_type: 'all',
      target_id: null,
      rpm_limit: 25,
      enabled: true
    })
  })

  it('rpm_limit 为 0 时拒绝提交（这里的 0 不是免检绿灯）', async () => {
    const wrapper = mountModal()
    await wrapper.get('#model-rpm-rule-name').setValue('n')
    await wrapper.get('#model-rpm-rule-pattern').setValue('m')
    await wrapper.get('#model-rpm-rule-limit').setValue('0')

    await saveButton(wrapper).trigger('click')
    await flushPromises()

    expect(createRule).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('admin.modelRpmRules.errors.rpmLimitPositive')
  })

  it('中缀通配符被拒绝', async () => {
    const wrapper = mountModal()
    await wrapper.get('#model-rpm-rule-name').setValue('n')
    await wrapper.get('#model-rpm-rule-pattern').setValue('cla*de')

    await saveButton(wrapper).trigger('click')
    await flushPromises()

    expect(createRule).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('admin.modelRpmRules.errors.modelPatternWildcard')
  })

  it('适用范围为分组但未选目标时拒绝提交', async () => {
    const wrapper = mountModal()
    await wrapper.get('#model-rpm-rule-name').setValue('n')
    await wrapper.get('#model-rpm-rule-pattern').setValue('m')
    await wrapper.get('#model-rpm-rule-target-type').setValue('group')

    await saveButton(wrapper).trigger('click')
    await flushPromises()

    expect(createRule).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('admin.modelRpmRules.errors.targetRequired')
  })

  it('目标切到单个用户时 scope 锁定为 user', async () => {
    const wrapper = mountModal()
    await wrapper.get('#model-rpm-rule-scope').setValue('global')
    await wrapper.get('#model-rpm-rule-target-type').setValue('user')
    await flushPromises()

    const scopeSelect = wrapper.get('#model-rpm-rule-scope')
    expect((scopeSelect.element as HTMLSelectElement).value).toBe('user')
    expect((scopeSelect.element as HTMLSelectElement).disabled).toBe(true)
  })

  it('编辑现有规则时回填并走 update', async () => {
    const rule: ModelRPMRule = {
      id: 5,
      name: 'vip',
      model_pattern: 'gpt-5',
      scope: 'user',
      target_type: 'group',
      target_id: 7,
      rpm_limit: 30,
      enabled: false,
      created_at: '',
      updated_at: '',
      target_name: 'vip'
    }
    const wrapper = mountModal(rule)
    await flushPromises()

    expect((wrapper.get('#model-rpm-rule-name').element as HTMLInputElement).value).toBe('vip')
    expect((wrapper.get('#model-rpm-rule-group').element as HTMLSelectElement).value).toBe('7')

    await wrapper.get('#model-rpm-rule-limit').setValue('50')
    await saveButton(wrapper).trigger('click')
    await flushPromises()

    expect(updateRule).toHaveBeenCalledWith(5, expect.objectContaining({ rpm_limit: 50, target_id: 7 }))
    expect(wrapper.emitted('saved')).toBeTruthy()
  })
})
