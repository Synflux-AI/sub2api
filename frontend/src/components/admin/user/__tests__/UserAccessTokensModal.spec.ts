import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import UserAccessTokensModal from '../UserAccessTokensModal.vue'

const { getUserAccessToken, rotateUserAccessToken, revokeUserAccessToken, showSuccess, showError, copyToClipboard } =
  vi.hoisted(() => ({
    getUserAccessToken: vi.fn(),
    rotateUserAccessToken: vi.fn(),
    revokeUserAccessToken: vi.fn(),
    showSuccess: vi.fn(),
    showError: vi.fn(),
    copyToClipboard: vi.fn()
  }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accessTokens: {
      getUserAccessToken,
      rotateUserAccessToken,
      revokeUserAccessToken
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showSuccess,
    showError
  })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key)
    })
  }
})

const EMPTY_TOKEN = { token: null, created_at: null, last_used_at: null }
const ACTIVE_TOKEN = {
  token: 'sat-plaintext-abc123',
  created_at: '2026-07-01T00:00:00Z',
  last_used_at: null
}
const ROTATED_TOKEN = {
  token: 'sat-plaintext-def456',
  created_at: '2026-07-30T00:00:00Z',
  last_used_at: null
}

function makeUser(overrides: Record<string, unknown> = {}) {
  return { id: 42, email: 'admin-target@example.com', username: 'target', ...overrides } as any
}

/**
 * `show` starts at `false` and is flipped to `true` after mount — the modal's
 * `watch(() => props.show, ...)` only fires on a value *change* (no
 * `immediate: true`), matching how the real UsersView.vue toggles it. This
 * mirrors the existing `mountAndOpen` convention in
 * UserPlatformQuotaModal.spec.ts.
 */
async function mountModal(props: Record<string, unknown> = {}) {
  const wrapper = mount(UserAccessTokensModal, {
    props: {
      show: false,
      user: makeUser(),
      ...props
    },
    global: {
      stubs: {
        BaseDialog: {
          props: ['show', 'title', 'width'],
          emits: ['close'],
          template: '<div v-if="show"><slot /><slot name="footer" /></div>'
        },
        ConfirmDialog: {
          props: ['show', 'title', 'message', 'danger'],
          emits: ['confirm', 'cancel'],
          template:
            '<div v-if="show"><button data-testid="confirm-dialog-confirm" @click="$emit(\'confirm\')">confirm</button><button data-testid="confirm-dialog-cancel" @click="$emit(\'cancel\')">cancel</button></div>'
        },
        TotpStepUpDialog: { props: ['controller'], template: '<div />' },
        Icon: true
      }
    }
  })
  await wrapper.setProps({ show: true })
  return wrapper
}

describe('UserAccessTokensModal', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getUserAccessToken.mockResolvedValue(EMPTY_TOKEN)
    rotateUserAccessToken.mockResolvedValue(ACTIVE_TOKEN)
    revokeUserAccessToken.mockResolvedValue(undefined)
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('loads and displays the target user token when opened (view)', async () => {
    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    // 管理端不需要目标用户密码：GET 只传 userId，不带任何 body/password 参数
    expect(getUserAccessToken).toHaveBeenCalledWith(42)
    expect(getUserAccessToken).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="admin-access-token-value"]').text()).toContain('sat-plaintext-abc123')
  })

  it('shows the empty state when the user has no token yet', async () => {
    getUserAccessToken.mockResolvedValueOnce(EMPTY_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    expect(wrapper.find('[data-testid="admin-access-token-empty"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="admin-access-token-value"]').exists()).toBe(false)
  })

  it('generates a token for a user who has none, with no password required', async () => {
    getUserAccessToken.mockResolvedValueOnce(EMPTY_TOKEN)
    rotateUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    const generateButton = wrapper.get('[data-testid="admin-access-token-generate"]')
    await generateButton.trigger('click')
    await flushPromises()

    expect(rotateUserAccessToken).toHaveBeenCalledWith(42)
    expect(rotateUserAccessToken).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="admin-access-token-value"]').text()).toContain('sat-plaintext-abc123')
    expect(showSuccess).toHaveBeenCalled()
  })

  it('rotates (regenerates) an existing token', async () => {
    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    rotateUserAccessToken.mockResolvedValueOnce(ROTATED_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    expect(wrapper.get('[data-testid="admin-access-token-value"]').text()).toContain('sat-plaintext-abc123')

    await wrapper.get('[data-testid="admin-access-token-generate"]').trigger('click')
    await flushPromises()

    expect(rotateUserAccessToken).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-testid="admin-access-token-value"]').text()).toContain('sat-plaintext-def456')
    expect(wrapper.get('[data-testid="admin-access-token-value"]').text()).not.toContain('sat-plaintext-abc123')
  })

  it('revokes a token after confirming, with no password required', async () => {
    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    await wrapper.get('[data-testid="admin-access-token-revoke"]').trigger('click')
    expect((wrapper.vm as any).showRevokeConfirm).toBe(true)

    await wrapper.get('[data-testid="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()

    expect(revokeUserAccessToken).toHaveBeenCalledWith(42)
    expect(revokeUserAccessToken).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-testid="admin-access-token-empty"]').exists()).toBe(true)
    expect(showSuccess).toHaveBeenCalled()
  })

  it('cancelling the revoke confirmation leaves the token untouched', async () => {
    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    await wrapper.get('[data-testid="admin-access-token-revoke"]').trigger('click')
    await wrapper.get('[data-testid="confirm-dialog-cancel"]').trigger('click')

    expect(revokeUserAccessToken).not.toHaveBeenCalled()
    expect((wrapper.vm as any).showRevokeConfirm).toBe(false)
    expect(wrapper.get('[data-testid="admin-access-token-value"]').text()).toContain('sat-plaintext-abc123')
  })

  it('clears in-memory token and modal state after the dialog is closed', async () => {
    // 直接断言组件内部状态（通过 defineExpose），而不是关闭后 DOM 找不到元素——
    // BaseDialog 用 v-if 控制可见性，DOM 卸载不代表这个组件实例被销毁，
    // 明文令牌 ref 如果不显式清空，会一直留在内存里，下次打开（哪怕换了别的用户）
    // 时可能在新请求返回前先闪现出上一个用户的令牌。
    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    expect((wrapper.vm as any).token).toEqual(ACTIVE_TOKEN)

    // 打开撤销确认框，验证它也会被关闭时的清理逻辑复位
    await wrapper.get('[data-testid="admin-access-token-revoke"]').trigger('click')
    expect((wrapper.vm as any).showRevokeConfirm).toBe(true)

    await wrapper.setProps({ show: false })

    expect((wrapper.vm as any).token).toBeNull()
    expect((wrapper.vm as any).loading).toBe(false)
    expect((wrapper.vm as any).loadFailed).toBe(false)
    expect((wrapper.vm as any).actionBusy).toBe(false)
    expect((wrapper.vm as any).showRevokeConfirm).toBe(false)
  })

  it('discards a load response that resolves after the modal has already been closed (stale-request guard)', async () => {
    // Traces the reviewer-found path: TotpStepUpDialog sits under BaseDialog's
    // document-level Escape handler (closeOnEscape defaults true) with no
    // stopPropagation, so Escape while a TOTP prompt is up for user A closes
    // the underlying modal. Without a sequence guard, `token.value = await
    // stepUp.run(...)` resolving afterwards would re-populate the (supposedly
    // cleared) plaintext token in a closed modal's memory.
    let resolveGet!: (value: typeof ACTIVE_TOKEN) => void
    getUserAccessToken.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveGet = resolve
        })
    )
    const wrapper = await mountModal()
    await flushPromises()
    expect((wrapper.vm as any).loading).toBe(true)

    // Close before the in-flight request resolves.
    await wrapper.setProps({ show: false })
    expect((wrapper.vm as any).token).toBeNull()
    expect((wrapper.vm as any).loading).toBe(false)

    // Now let the stale request resolve.
    resolveGet(ACTIVE_TOKEN)
    await flushPromises()

    expect((wrapper.vm as any).token).toBeNull()
    expect((wrapper.vm as any).loading).toBe(false)
  })

  it('re-fetches a fresh token when reopened for a different user instead of reusing stale state', async () => {
    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    const wrapper = await mountModal({ user: makeUser({ id: 42 }) })
    await flushPromises()
    expect((wrapper.vm as any).token).toEqual(ACTIVE_TOKEN)

    await wrapper.setProps({ show: false })
    expect((wrapper.vm as any).token).toBeNull()

    getUserAccessToken.mockResolvedValueOnce(EMPTY_TOKEN)
    await wrapper.setProps({ show: true, user: makeUser({ id: 99 }) })
    await flushPromises()

    expect(getUserAccessToken).toHaveBeenLastCalledWith(99)
    expect((wrapper.vm as any).token).toEqual(EMPTY_TOKEN)
  })

  it('never falls into the empty state on a load failure (would invite an accidental rotate of a live token)', async () => {
    getUserAccessToken.mockRejectedValueOnce(new Error('network down'))
    const wrapper = await mountModal()
    await flushPromises()

    expect(wrapper.find('[data-testid="admin-access-token-load-error"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="admin-access-token-empty"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="admin-access-token-generate"]').exists()).toBe(false)
    expect(showError).toHaveBeenCalled()

    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    await wrapper.get('[data-testid="admin-access-token-retry"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="admin-access-token-load-error"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="admin-access-token-value"]').text()).toContain('sat-plaintext-abc123')
  })

  it('shows a localized message (not the raw backend string) when the target user no longer exists (404)', async () => {
    // Backend admin.go resolveTarget returns a bare 404 with message "User not
    // found" and no `reason`/`code` marker — must not be shown to the admin
    // verbatim in English regardless of locale.
    getUserAccessToken.mockRejectedValueOnce({ status: 404, message: 'User not found' })
    const wrapper = await mountModal()
    await flushPromises()

    expect(wrapper.find('[data-testid="admin-access-token-load-error"]').exists()).toBe(true)
    expect(showError).toHaveBeenCalledWith('admin.users.accessToken.userNotFound')
    expect(showError).not.toHaveBeenCalledWith('User not found')
  })

  it('copies the plaintext token via the clipboard composable without touching localStorage/sessionStorage', async () => {
    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    const copyButton = wrapper.findAll('button').find((b) => b.text().includes('common.copy'))
    expect(copyButton).toBeTruthy()
    await copyButton!.trigger('click')

    expect(copyToClipboard).toHaveBeenCalledWith('sat-plaintext-abc123')
  })

  it('never persists the plaintext token to localStorage/sessionStorage or logs it to the console', async () => {
    const localSetItem = vi.spyOn(window.localStorage.__proto__, 'setItem')
    const sessionSetItem = vi.spyOn(window.sessionStorage.__proto__, 'setItem')
    const consoleSpies = {
      log: vi.spyOn(console, 'log').mockImplementation(() => {}),
      warn: vi.spyOn(console, 'warn').mockImplementation(() => {}),
      error: vi.spyOn(console, 'error').mockImplementation(() => {}),
      info: vi.spyOn(console, 'info').mockImplementation(() => {}),
      debug: vi.spyOn(console, 'debug').mockImplementation(() => {})
    }

    const touchedToken = (calls: unknown[][]) =>
      calls.some((args) => args.some((arg) => typeof arg === 'string' && arg.includes('sat-plaintext')))

    // Canary: this is a pure-negative assertion below (nothing contains the
    // token), which passes trivially if the spies never fire at all. Prove
    // each spy actually observes a matching write *before* trusting the
    // negative assertions, so this test can genuinely fail if the component
    // ever leaks the token through one of these channels. (localStorage and
    // sessionStorage share the same Storage.prototype in this test
    // environment, so both spies see both canary writes — clear both after.)
    window.localStorage.setItem('canary', 'sat-plaintext-canary-proof')
    window.sessionStorage.setItem('canary', 'sat-plaintext-canary-proof')
    expect(touchedToken(localSetItem.mock.calls)).toBe(true)
    expect(touchedToken(sessionSetItem.mock.calls)).toBe(true)
    localSetItem.mockClear()
    sessionSetItem.mockClear()
    window.localStorage.removeItem('canary')
    window.sessionStorage.removeItem('canary')

    getUserAccessToken.mockResolvedValueOnce(ACTIVE_TOKEN)
    rotateUserAccessToken.mockResolvedValueOnce(ROTATED_TOKEN)
    const wrapper = await mountModal()
    await flushPromises()

    // view -> generate/rotate -> copy -> revoke: full lifecycle of the plaintext token.
    // (copyToClipboard itself is mocked at module level for every test in this file —
    // this test asserts the *component* never writes the plaintext directly through
    // these channels; the shared clipboard composable's own hygiene is a separate,
    // already-generic utility outside this component's responsibility.)
    await wrapper.get('[data-testid="admin-access-token-generate"]').trigger('click')
    await flushPromises()
    const copyButton = wrapper.findAll('button').find((b) => b.text().includes('common.copy'))
    await copyButton!.trigger('click')
    await wrapper.get('[data-testid="admin-access-token-revoke"]').trigger('click')
    await wrapper.get('[data-testid="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()

    expect(touchedToken(localSetItem.mock.calls)).toBe(false)
    expect(touchedToken(sessionSetItem.mock.calls)).toBe(false)
    for (const [name, spy] of Object.entries(consoleSpies)) {
      expect(touchedToken(spy.mock.calls), `console.${name} must never log the plaintext token`).toBe(false)
    }

    localSetItem.mockRestore()
    sessionSetItem.mockRestore()
    for (const spy of Object.values(consoleSpies)) spy.mockRestore()
  })
})
