<template>
  <div class="card">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-medium text-gray-900 dark:text-white">
        {{ t('profile.accessToken.title') }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t('profile.accessToken.description') }}
      </p>
    </div>

    <div class="px-6 py-6">
      <div v-if="loading" class="flex justify-center py-6">
        <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-500"></div>
      </div>

      <!--
        载入失败时绝不能落入「未生成」空态：空态紧挨着「生成令牌」按钮，
        而生成/轮换用的是同一个 rotate 接口——一旦用户在真实令牌仍然有效时
        误按生成，会立刻让第三方集成正在使用的令牌失效。载入失败必须停在
        一个独立的、不可操作 rotate/revoke 的重试态上。
      -->
      <div
        v-else-if="loadFailed"
        data-testid="access-token-load-error"
        class="rounded-lg border border-dashed border-red-200 px-4 py-8 text-center text-sm text-red-600 dark:border-red-900/40 dark:text-red-400"
      >
        <p>{{ t('profile.accessToken.loadFailed') }}</p>
        <button
          type="button"
          class="btn btn-secondary btn-sm mt-3"
          @click="retryLoad"
        >
          {{ t('profile.accessToken.loadErrorRetry') }}
        </button>
      </div>

      <template v-else>
        <div
          v-if="!token || !token.token"
          data-testid="access-token-empty"
          class="rounded-lg border border-dashed border-gray-200 px-4 py-8 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-gray-400"
        >
          {{ t('profile.accessToken.empty') }}
        </div>

        <div v-else class="space-y-3">
          <div class="flex items-center gap-2">
            <code
              data-testid="access-token-value"
              class="min-w-0 flex-1 truncate rounded bg-gray-100 px-3 py-2 font-mono text-sm dark:bg-dark-700"
            >
              {{ token.token }}
            </code>
            <button type="button" class="btn btn-secondary btn-sm shrink-0" @click="copyToken">
              <Icon name="copy" size="sm" />
              {{ t('common.copy') }}
            </button>
          </div>
          <p class="text-xs text-gray-500 dark:text-gray-400">
            {{ t('profile.accessToken.createdAt', { date: formatDateTime(token.created_at) }) }}
            <template v-if="token.last_used_at">
              · {{ t('profile.accessToken.lastUsed', { date: formatDateTime(token.last_used_at) }) }}
            </template>
          </p>
        </div>

        <!-- 生成/重新生成：与撤销一样需要账户密码二次确认，内联展开，不用弹窗 -->
        <form
          v-if="showRotateForm"
          class="mt-4 flex flex-col gap-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700"
          @submit.prevent="confirmRotate"
        >
          <div>
            <label for="access-token-rotate-password" class="input-label">{{
              t('profile.currentPassword')
            }}</label>
            <input
              id="access-token-rotate-password"
              v-model="rotatePassword"
              type="password"
              autocomplete="current-password"
              class="input"
              :placeholder="t('profile.accessToken.passwordPlaceholder')"
              autofocus
            />
          </div>
          <div class="flex justify-end gap-2">
            <button type="button" class="btn btn-secondary" :disabled="busy" @click="cancelRotate">
              {{ t('common.cancel') }}
            </button>
            <button
              type="submit"
              class="btn btn-primary"
              :disabled="busy || rotatePassword.length === 0"
            >
              {{ busy ? t('common.processing') : t('common.confirm') }}
            </button>
          </div>
        </form>

        <div v-else class="mt-4 flex flex-wrap gap-2">
          <button type="button" class="btn btn-primary" :disabled="busy" @click="openRotateForm">
            {{ token && token.token ? t('profile.accessToken.regenerate') : t('profile.accessToken.generate') }}
          </button>
          <button
            v-if="token && token.token"
            type="button"
            class="btn btn-ghost text-red-600 hover:bg-red-50 dark:text-red-300 dark:hover:bg-red-950/30"
            :disabled="busy"
            @click="openRevokeDialog"
          >
            {{ t('profile.accessToken.revoke') }}
          </button>
        </div>
      </template>
    </div>

    <!-- 撤销确认：撤销令牌需验证当前密码，防止被窃会话静默失效他人依赖的余额查询集成 -->
    <div v-if="showRevokeDialog" class="fixed inset-0 z-50 overflow-y-auto">
      <div class="flex min-h-full items-center justify-center p-4">
        <div class="fixed inset-0 bg-black/50 transition-opacity" @click="closeRevokeDialog"></div>
        <div
          class="relative w-full max-w-md transform rounded-xl bg-white p-6 shadow-xl transition-all dark:bg-dark-800"
        >
          <h3 class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('profile.accessToken.revokeTitle') }}
          </h3>
          <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">
            {{ t('profile.accessToken.revokeConfirm') }}
          </p>
          <form class="mt-4 space-y-4" @submit.prevent="confirmRevoke">
            <div>
              <label for="access-token-revoke-password" class="input-label">{{
                t('profile.currentPassword')
              }}</label>
              <input
                id="access-token-revoke-password"
                v-model="revokePassword"
                type="password"
                autocomplete="current-password"
                class="input"
                :placeholder="t('profile.accessToken.passwordPlaceholder')"
                autofocus
              />
            </div>
            <div class="flex justify-end gap-3">
              <button
                type="button"
                class="btn btn-secondary"
                :disabled="busy"
                @click="closeRevokeDialog"
              >
                {{ t('common.cancel') }}
              </button>
              <button
                type="submit"
                class="btn btn-danger"
                :disabled="busy || revokePassword.length === 0"
              >
                {{ busy ? t('common.processing') : t('profile.accessToken.revoke') }}
              </button>
            </div>
          </form>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { accessTokenAPI, type UserAccessToken } from '@/api'
import { Icon } from '@/components/icons'
import { useAppStore } from '@/stores/app'
import { useClipboard } from '@/composables/useClipboard'
import { formatDateTime } from '@/utils/format'

const { t } = useI18n()
const appStore = useAppStore()
const { copyToClipboard } = useClipboard()

const loading = ref(false)
const loadFailed = ref(false)
const busy = ref(false)
const token = ref<UserAccessToken | null>(null)

const showRotateForm = ref(false)
const rotatePassword = ref('')

const showRevokeDialog = ref(false)
const revokePassword = ref('')

// apiClient 拦截器把错误规范化为 { code, reason, message }；
// 透出后端消息，否则回退到通用文案。
function extractErrorMessage(error: unknown, fallback: string): string {
  const message = (error as { message?: string }).message
  return typeof message === 'string' && message.length > 0 ? message : fallback
}

// 密码错误 / 缺失密码需要给出可操作提示，而不是通用的“密码错误”：
// OAuth 首次注册账号的本地密码是系统随机值，用户并不知道，必须先重置密码或找管理员代办。
function reportPasswordGatedFailure(error: unknown, fallback: string): void {
  const reason = (error as { reason?: string }).reason
  if (reason === 'ACCESS_TOKEN_PASSWORD_INCORRECT') {
    appStore.showError(t('profile.accessToken.oauthPasswordHint'))
    return
  }
  if (reason === 'PASSWORD_REQUIRED') {
    appStore.showError(t('profile.accessToken.passwordRequiredHint'))
    return
  }
  appStore.showError(extractErrorMessage(error, fallback))
}

async function loadToken(): Promise<void> {
  loading.value = true
  loadFailed.value = false
  try {
    token.value = await accessTokenAPI.getAccessToken()
  } catch {
    // 不设 token.value：保留失败前的未知状态，交给 loadFailed 渲染独立的
    // 重试态，避免落入「未生成」空态并暴露可操作的生成/轮换按钮。
    loadFailed.value = true
    appStore.showError(t('profile.accessToken.loadFailed'))
  } finally {
    loading.value = false
  }
}

function retryLoad(): void {
  void loadToken()
}

function openRotateForm(): void {
  showRotateForm.value = true
}

function cancelRotate(): void {
  showRotateForm.value = false
  rotatePassword.value = ''
}

async function confirmRotate(): Promise<void> {
  if (rotatePassword.value.length === 0) return
  const wasFirstGeneration = !token.value || !token.value.token
  busy.value = true
  try {
    token.value = await accessTokenAPI.rotateAccessToken(rotatePassword.value)
    appStore.showSuccess(
      wasFirstGeneration
        ? t('profile.accessToken.generateSuccess')
        : t('profile.accessToken.rotateSuccess')
    )
    cancelRotate()
  } catch (error) {
    // 密码错误等失败保持表单打开，允许重试
    reportPasswordGatedFailure(
      error,
      wasFirstGeneration
        ? t('profile.accessToken.generateFailed')
        : t('profile.accessToken.rotateFailed')
    )
  } finally {
    busy.value = false
  }
}

function openRevokeDialog(): void {
  showRevokeDialog.value = true
}

function closeRevokeDialog(): void {
  showRevokeDialog.value = false
  revokePassword.value = ''
}

async function confirmRevoke(): Promise<void> {
  if (revokePassword.value.length === 0) return
  busy.value = true
  try {
    await accessTokenAPI.revokeAccessToken(revokePassword.value)
    token.value = { token: null, created_at: null, last_used_at: null }
    appStore.showSuccess(t('profile.accessToken.revokeSuccess'))
    closeRevokeDialog()
  } catch (error) {
    // 密码错误等失败保持对话框打开，允许重试
    reportPasswordGatedFailure(error, t('profile.accessToken.revokeFailed'))
  } finally {
    busy.value = false
  }
}

async function copyToken(): Promise<void> {
  if (!token.value?.token) return
  await copyToClipboard(token.value.token)
}

onMounted(() => {
  void loadToken()
})
</script>
