import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import UserApiKeysModal from '../UserApiKeysModal.vue'

const { getUserApiKeys, getAllGroups, updateApiKeyGroups, showSuccess, showError } = vi.hoisted(() => ({
  getUserApiKeys: vi.fn(),
  getAllGroups: vi.fn(),
  updateApiKeyGroups: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    users: { getUserApiKeys },
    groups: { getAll: getAllGroups },
    apiKeys: { updateApiKeyGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess, showError })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key
    })
  }
})

const anthropic = {
  id: 10,
  name: 'claude-ccmax',
  platform: 'anthropic',
  rate_multiplier: 1.0,
  subscription_type: 'standard',
  status: 'active',
  description: ''
}
const openai = {
  id: 20,
  name: 'codex',
  platform: 'openai',
  rate_multiplier: 1.2,
  subscription_type: 'standard',
  status: 'active',
  description: ''
}

function multiGroupKey() {
  return {
    id: 1,
    user_id: 7,
    key: 'sk-x',
    name: 'k1',
    status: 'active',
    group_id: 10,
    group: anthropic,
    group_ids: [10, 20],
    groups: [anthropic, openai],
    created_at: '2025-01-01T00:00:00Z'
  }
}

async function mountModal(keys: unknown[]) {
  getUserApiKeys.mockResolvedValue({ items: keys })
  getAllGroups.mockResolvedValue([anthropic, openai])
  // 组件是在 show 由 false 变 true 时才加载数据的（watch 没有 immediate），
  // 所以必须先挂成关闭态再打开，否则列表永远是空的。
  const wrapper = mount(UserApiKeysModal, {
    props: { show: false, user: { id: 7, email: 'u@test.com' } as never },
    global: { stubs: { Teleport: true } }
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

/**
 * issue #171：管理端这个弹窗从「单选替换」变成了「集合切换」。
 *
 * 两个行为变化必须被钉住：
 *  1. 展示的是**全部**绑定分组，不再只有默认组；
 *  2. 点某个分组是**切换**（加入/移除）后整体提交集合，而不是把集合替换成那一个。
 *     写错成替换的话，管理员想给多分组 Key 加一个平台，结果会把其它平台的绑定删掉。
 */
describe('UserApiKeysModal 的多分组绑定', () => {
  beforeEach(() => {
    getUserApiKeys.mockReset()
    getAllGroups.mockReset()
    updateApiKeyGroups.mockReset().mockImplementation((_id: number, groupIds: number[]) =>
      Promise.resolve({
        api_key: { ...multiGroupKey(), group_ids: groupIds },
        auto_granted_group_access: false
      })
    )
    showSuccess.mockReset()
    showError.mockReset()
  })

  it('展示全部绑定分组的名称，而不只是默认组', async () => {
    const wrapper = await mountModal([multiGroupKey()])
    const text = wrapper.text()
    expect(text).toContain('claude-ccmax')
    expect(text).toContain('codex')
  })

  it('老响应体只有单个 group 时退回展示它，不显示成「无」', async () => {
    const legacy = { ...multiGroupKey(), group_ids: [], groups: undefined }
    const wrapper = await mountModal([legacy])
    expect(wrapper.text()).toContain('claude-ccmax')
  })

  it('点已绑定的分组是**移除**，提交剩下的集合而不是替换成它', async () => {
    const wrapper = await mountModal([multiGroupKey()])
    const vm = wrapper.vm as unknown as {
      toggleGroup: (k: unknown, id: number) => Promise<void>
    }
    await vm.toggleGroup(multiGroupKey(), 20)

    expect(updateApiKeyGroups).toHaveBeenCalledTimes(1)
    expect(updateApiKeyGroups.mock.calls[0][1]).toEqual([10])
  })

  it('点未绑定的分组是**加入**，保留原有绑定', async () => {
    const single = { ...multiGroupKey(), group_ids: [10], groups: [anthropic] }
    const wrapper = await mountModal([single])
    const vm = wrapper.vm as unknown as {
      toggleGroup: (k: unknown, id: number) => Promise<void>
    }
    await vm.toggleGroup(single, 20)

    expect(updateApiKeyGroups.mock.calls[0][1]).toEqual([10, 20])
  })

  it('解绑发空数组（显式清空），而不是省略字段', async () => {
    const wrapper = await mountModal([multiGroupKey()])
    const vm = wrapper.vm as unknown as {
      clearGroups: (k: unknown) => Promise<void>
    }
    await vm.clearGroups(multiGroupKey())

    expect(updateApiKeyGroups.mock.calls[0][1]).toEqual([])
  })

  it('已经是空绑定时再点解绑不发请求', async () => {
    const unbound = { ...multiGroupKey(), group_id: null, group: undefined, group_ids: [], groups: [] }
    const wrapper = await mountModal([unbound])
    const vm = wrapper.vm as unknown as {
      clearGroups: (k: unknown) => Promise<void>
    }
    await vm.clearGroups(unbound)

    expect(updateApiKeyGroups).not.toHaveBeenCalled()
  })

  it('后端返回校验错误（如同平台冲突）时展示它的消息，前端不自己判定', async () => {
    updateApiKeyGroups.mockRejectedValueOnce(new Error('同一平台最多只能绑定一个分组'))
    const wrapper = await mountModal([multiGroupKey()])
    const vm = wrapper.vm as unknown as {
      toggleGroup: (k: unknown, id: number) => Promise<void>
    }
    await vm.toggleGroup(multiGroupKey(), 20)

    expect(showError).toHaveBeenCalledWith('同一平台最多只能绑定一个分组')
  })
})
