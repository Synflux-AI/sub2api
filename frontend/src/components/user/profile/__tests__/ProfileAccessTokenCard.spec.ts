import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ProfileAccessTokenCard from '@/components/user/profile/ProfileAccessTokenCard.vue'

const {
  getAccessTokenMock,
  rotateAccessTokenMock,
  revokeAccessTokenMock,
  copyToClipboardMock,
  showSuccessMock,
  showErrorMock
} = vi.hoisted(() => ({
  getAccessTokenMock: vi.fn(),
  rotateAccessTokenMock: vi.fn(),
  revokeAccessTokenMock: vi.fn(),
  copyToClipboardMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn()
}))

vi.mock('@/api', () => ({
  accessTokenAPI: {
    getAccessToken: getAccessTokenMock,
    rotateAccessToken: rotateAccessTokenMock,
    revokeAccessToken: revokeAccessTokenMock
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showSuccess: showSuccessMock,
    showError: showErrorMock
  })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: copyToClipboardMock })
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    // 断言原始 key 字符串（本仓同类卡片测试的惯例，见 ProfilePasswordForm.spec.ts 的姊妹文件）
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

function mountCard() {
  return mount(ProfileAccessTokenCard, {
    global: {
      stubs: {
        Icon: true
      }
    }
  })
}

const EMPTY_TOKEN = { token: null, created_at: null, last_used_at: null }
const ACTIVE_TOKEN = {
  token: 'sat-plaintext-abc123',
  created_at: '2026-07-01T00:00:00Z',
  last_used_at: '2026-07-15T00:00:00Z'
}

describe('ProfileAccessTokenCard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('shows the empty state and a generate path when no token exists yet', async () => {
    getAccessTokenMock.mockResolvedValue(EMPTY_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    expect(wrapper.get('[data-testid="access-token-empty"]').text()).toBe('profile.accessToken.empty')
    expect(wrapper.find('[data-testid="access-token-value"]').exists()).toBe(false)

    const generateButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'profile.accessToken.generate')
    expect(generateButton).toBeTruthy()

    await generateButton!.trigger('click')
    await wrapper.get('#access-token-rotate-password').setValue('correct-password')
    rotateAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(rotateAccessTokenMock).toHaveBeenCalledWith('correct-password')
    expect(showSuccessMock).toHaveBeenCalledWith('profile.accessToken.generateSuccess')
    expect(wrapper.get('[data-testid="access-token-value"]').text()).toContain('sat-plaintext-abc123')
  })

  it('displays the plaintext token and copies it to the clipboard', async () => {
    getAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    expect(wrapper.get('[data-testid="access-token-value"]').text()).toContain('sat-plaintext-abc123')

    const copyButton = wrapper.findAll('button').find((button) => button.text().includes('common.copy'))
    expect(copyButton).toBeTruthy()
    await copyButton!.trigger('click')

    expect(copyToClipboardMock).toHaveBeenCalledWith('sat-plaintext-abc123')
  })

  it('requires a password before rotating an existing token', async () => {
    getAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    const regenerateButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'profile.accessToken.regenerate')
    await regenerateButton!.trigger('click')

    // 空密码：提交按钮禁用，提交不应发起请求
    const submitButton = wrapper.get('#access-token-rotate-password').element
      .closest('form')!
      .querySelector('button[type="submit"]') as HTMLButtonElement
    expect(submitButton.disabled).toBe(true)

    await wrapper.get('form').trigger('submit.prevent')
    expect(rotateAccessTokenMock).not.toHaveBeenCalled()

    await wrapper.get('#access-token-rotate-password').setValue('my-password')
    rotateAccessTokenMock.mockResolvedValue({ ...ACTIVE_TOKEN, token: 'sat-rotated' })
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(rotateAccessTokenMock).toHaveBeenCalledWith('my-password')
  })

  it('requires a password to revoke and returns to the empty state afterwards', async () => {
    getAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    const revokeButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'profile.accessToken.revoke')
    await revokeButton!.trigger('click')

    const revokeSubmit = wrapper.get('#access-token-revoke-password').element
      .closest('form')!
      .querySelector('button[type="submit"]') as HTMLButtonElement
    expect(revokeSubmit.disabled).toBe(true)

    await wrapper.get('form').trigger('submit.prevent')
    expect(revokeAccessTokenMock).not.toHaveBeenCalled()

    await wrapper.get('#access-token-revoke-password').setValue('my-password')
    revokeAccessTokenMock.mockResolvedValue(undefined)
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(revokeAccessTokenMock).toHaveBeenCalledWith('my-password')
    expect(showSuccessMock).toHaveBeenCalledWith('profile.accessToken.revokeSuccess')
    expect(wrapper.get('[data-testid="access-token-empty"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="access-token-value"]').exists()).toBe(false)
  })

  it('shows OAuth/no-local-password guidance instead of a generic error on wrong password', async () => {
    getAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    const regenerateButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'profile.accessToken.regenerate')
    await regenerateButton!.trigger('click')
    await wrapper.get('#access-token-rotate-password').setValue('wrong-password')

    rotateAccessTokenMock.mockRejectedValue({
      status: 403,
      reason: 'ACCESS_TOKEN_PASSWORD_INCORRECT',
      message: 'Incorrect password'
    })
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    // 必须是 OAuth 可操作提示的 key，而不是通用错误文案
    expect(showErrorMock).toHaveBeenCalledWith('profile.accessToken.oauthPasswordHint')
    expect(showErrorMock).not.toHaveBeenCalledWith('profile.accessToken.rotateFailed')
    // 失败后表单保持打开，允许重试
    expect(wrapper.find('#access-token-rotate-password').exists()).toBe(true)
  })

  it('shows a dedicated hint when the password is missing', async () => {
    getAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    const revokeButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'profile.accessToken.revoke')
    await revokeButton!.trigger('click')
    await wrapper.get('#access-token-revoke-password').setValue('x')

    revokeAccessTokenMock.mockRejectedValue({
      status: 400,
      reason: 'PASSWORD_REQUIRED',
      message: 'Password required'
    })
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(showErrorMock).toHaveBeenCalledWith('profile.accessToken.passwordRequiredHint')
  })

  it('clears the rotate password field after the inline form is closed', async () => {
    getAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    const regenerateButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'profile.accessToken.regenerate')
    await regenerateButton!.trigger('click')
    await wrapper.get('#access-token-rotate-password').setValue('leftover-secret')

    const cancelButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'common.cancel')
    await cancelButton!.trigger('click')

    // 表单关闭后重新打开：若密码状态未被清空，输入框会带着上次残留的值重新出现
    await regenerateButton!.trigger('click')
    const reopenedInput = wrapper.get('#access-token-rotate-password')
      .element as HTMLInputElement
    expect(reopenedInput.value).toBe('')
  })

  it('clears the revoke password field after the confirm modal is closed', async () => {
    getAccessTokenMock.mockResolvedValue(ACTIVE_TOKEN)
    const wrapper = mountCard()
    await flushPromises()

    const revokeButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'profile.accessToken.revoke')
    await revokeButton!.trigger('click')
    await wrapper.get('#access-token-revoke-password').setValue('leftover-secret')

    // 点击遮罩关闭弹窗
    const backdrop = wrapper.findAll('div').find((div) => div.classes().includes('bg-black/50'))
    expect(backdrop).toBeTruthy()
    await backdrop!.trigger('click')
    expect(wrapper.find('#access-token-revoke-password').exists()).toBe(false)

    await revokeButton!.trigger('click')
    const reopenedInput = wrapper.get('#access-token-revoke-password')
      .element as HTMLInputElement
    expect(reopenedInput.value).toBe('')
  })
})
