import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import KeysView from '../KeysView.vue'

const {
  listKeys,
  updateKey,
  getPublicSettings,
  getDashboardApiKeysUsage,
  getAvailableGroups,
  getUserGroupRates,
  showError,
  showSuccess,
} = vi.hoisted(() => ({
  listKeys: vi.fn(),
  updateKey: vi.fn(),
  getPublicSettings: vi.fn(),
  getDashboardApiKeysUsage: vi.fn(),
  getAvailableGroups: vi.fn(),
  getUserGroupRates: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api', () => ({
  keysAPI: {
    list: listKeys,
    create: vi.fn(),
    update: updateKey,
    delete: vi.fn(),
    toggleStatus: vi.fn(),
  },
  authAPI: { getPublicSettings },
  usageAPI: { getDashboardApiKeysUsage },
  // 组件实际用的是 userGroupsAPI.getAvailable / getUserGroupRates
  // （与 KeysView.spec.ts 的 mock 保持一致）。漏了会在控制台刷一屏 mock 缺失告警。
  userGroupsAPI: {
    getAvailable: getAvailableGroups,
    getUserGroupRates,
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess }),
}))

vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({ isCurrentStep: () => false, nextStep: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() }),
}))

const anthropic = {
  id: 10,
  name: 'claude-ccmax',
  platform: 'anthropic',
  rate_multiplier: 1.0,
  subscription_type: 'standard',
  status: 'active',
  description: '',
}
const openai = {
  id: 20,
  name: 'codex',
  platform: 'openai',
  rate_multiplier: 1.2,
  subscription_type: 'standard',
  status: 'active',
  description: '',
}

function multiGroupKey() {
  return {
    id: 1,
    user_id: 7,
    key: 'sk-abcdefghijklmnop',
    name: 'k1',
    status: 'active',
    group_id: 10,
    group: anthropic,
    group_ids: [10, 20],
    groups: [anthropic, openai],
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  }
}

async function mountView(keys: unknown[]) {
  listKeys.mockResolvedValue({ items: keys, total: keys.length, page: 1, page_size: 10, pages: 1 })
  getPublicSettings.mockResolvedValue({})
  getDashboardApiKeysUsage.mockResolvedValue({ items: [] })
  getAvailableGroups.mockResolvedValue([anthropic, openai])
  getUserGroupRates.mockResolvedValue({})
  const wrapper = mount(KeysView, {
    global: {
      stubs: {
        AppLayout: true,
        TablePageLayout: true,
        DataTable: true,
        Pagination: true,
        BaseDialog: true,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        MultiSelect: true,
        SearchInput: true,
        Icon: true,
        UseKeyModal: true,
        EndpointPopover: true,
        GroupBadge: true,
        GroupOptionItem: true,
        Teleport: true,
      },
    },
  })
  await flushPromises()
  await nextTick()
  return wrapper
}

/**
 * issue #171：用户端列表的行内「快捷改组」从单选替换改成了集合切换。
 *
 * 写成替换的话，用户想在列表里给多分组 Key 加一个平台，会顺手删掉它在其它平台上的
 * 绑定 —— 而列表里根本看不出发生了什么。这是本次最容易造成静默数据丢失的交互。
 */
describe('KeysView 行内分组切换（issue #171）', () => {
  beforeEach(() => {
    listKeys.mockReset()
    updateKey.mockReset().mockResolvedValue(multiGroupKey())
    getPublicSettings.mockReset()
    getDashboardApiKeysUsage.mockReset()
    getAvailableGroups.mockReset()
    getUserGroupRates.mockReset()
    showError.mockReset()
    showSuccess.mockReset()
  })

  it('boundGroupsOf 展示全部绑定分组', async () => {
    const wrapper = await mountView([multiGroupKey()])
    const vm = wrapper.vm as unknown as { boundGroupsOf: (k: unknown) => Array<{ id: number }> }
    expect(vm.boundGroupsOf(multiGroupKey()).map((g) => g.id)).toEqual([10, 20])
  })

  it('老响应体只有单个 group 时退回展示它', async () => {
    const legacy = { ...multiGroupKey(), group_ids: [], groups: undefined }
    const wrapper = await mountView([legacy])
    const vm = wrapper.vm as unknown as { boundGroupsOf: (k: unknown) => Array<{ id: number }> }
    expect(vm.boundGroupsOf(legacy).map((g) => g.id)).toEqual([10])
  })

  it('点已绑定的分组是移除，提交剩下的集合', async () => {
    const wrapper = await mountView([multiGroupKey()])
    const vm = wrapper.vm as unknown as {
      toggleRowGroup: (k: unknown, v: number | null) => Promise<void>
    }
    await vm.toggleRowGroup(multiGroupKey(), 20)

    expect(updateKey).toHaveBeenCalledTimes(1)
    expect(updateKey.mock.calls[0][1]).toEqual({ group_ids: [10] })
  })

  it('点未绑定的分组是加入，保留原有绑定（不得退化成替换）', async () => {
    const single = { ...multiGroupKey(), group_ids: [10], groups: [anthropic] }
    const wrapper = await mountView([single])
    const vm = wrapper.vm as unknown as {
      toggleRowGroup: (k: unknown, v: number | null) => Promise<void>
    }
    await vm.toggleRowGroup(single, 20)

    expect(updateKey.mock.calls[0][1]).toEqual({ group_ids: [10, 20] })
  })

  it('点「无分组」发空数组（显式解绑），而不是省略字段', async () => {
    const wrapper = await mountView([multiGroupKey()])
    const vm = wrapper.vm as unknown as {
      toggleRowGroup: (k: unknown, v: number | null) => Promise<void>
    }
    await vm.toggleRowGroup(multiGroupKey(), null)

    expect(updateKey.mock.calls[0][1]).toEqual({ group_ids: [] })
  })

  it('已经无分组时再点「无分组」不发请求', async () => {
    const unbound = { ...multiGroupKey(), group_id: null, group: undefined, group_ids: [], groups: [] }
    const wrapper = await mountView([unbound])
    const vm = wrapper.vm as unknown as {
      toggleRowGroup: (k: unknown, v: number | null) => Promise<void>
    }
    await vm.toggleRowGroup(unbound, null)

    expect(updateKey).not.toHaveBeenCalled()
  })

  it('后端校验错误（如同平台冲突）优先展示后端消息', async () => {
    updateKey.mockRejectedValueOnce(new Error('同一平台最多只能绑定一个分组'))
    const wrapper = await mountView([multiGroupKey()])
    const vm = wrapper.vm as unknown as {
      toggleRowGroup: (k: unknown, v: number | null) => Promise<void>
    }
    await vm.toggleRowGroup(multiGroupKey(), 20)

    expect(showError).toHaveBeenCalledWith('同一平台最多只能绑定一个分组')
  })

  // 下拉在勾选具体分组后保持打开，用户会连着点第二个平台；而列表数据要等
  // loadApiKeys() 回来才更新。第二次点击若仍从 key.group_ids 读起点，整体替换语义下
  // 就会把第一次刚加上的分组又提交掉 —— 而界面上完全看不出发生了什么。
  it('连续勾选两个平台时，第二次以在途集合为起点（不得丢掉第一次的改动）', async () => {
    const single = { ...multiGroupKey(), group_ids: [10], groups: [anthropic] }
    const wrapper = await mountView([single])
    const vm = wrapper.vm as unknown as {
      toggleRowGroup: (k: unknown, v: number | null) => Promise<void>
    }

    // 刻意不 await 第一次：模拟用户在请求回来之前就点了第二个。
    const first = vm.toggleRowGroup(single, 20)
    const second = vm.toggleRowGroup(single, 30)
    await Promise.all([first, second])

    expect(updateKey).toHaveBeenCalledTimes(2)
    expect(updateKey.mock.calls[0][1]).toEqual({ group_ids: [10, 20] })
    expect(updateKey.mock.calls[1][1]).toEqual({ group_ids: [10, 20, 30] })
  })

  // 在途集合只在提交序列内有效，落库（或失败）之后必须退回服务端事实，
  // 否则勾选态会永远停在一个可能没落库的集合上。
  it('提交结束后清空在途集合，勾选态回到服务端事实', async () => {
    const single = { ...multiGroupKey(), group_ids: [10], groups: [anthropic] }
    const wrapper = await mountView([single])
    const vm = wrapper.vm as unknown as {
      toggleRowGroup: (k: unknown, v: number | null) => Promise<void>
      rowGroupPending: { keyId: number; ids: number[] } | null
    }

    await vm.toggleRowGroup(single, 20)
    await flushPromises()

    expect(vm.rowGroupPending).toBeNull()
  })
})
