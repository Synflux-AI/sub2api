<template>
  <BaseDialog :show="show" :title="t('admin.users.accessToken.title')" width="normal" @close="handleClose">
    <div v-if="user" class="space-y-4">
      <div class="flex items-center gap-3 rounded-xl bg-gray-50 p-4 dark:bg-dark-700">
        <div class="flex h-10 w-10 items-center justify-center rounded-full bg-primary-100 dark:bg-primary-900/30">
          <span class="text-lg font-medium text-primary-700 dark:text-primary-300">{{ user.email.charAt(0).toUpperCase() }}</span>
        </div>
        <div><p class="font-medium text-gray-900 dark:text-white">{{ user.email }}</p><p class="text-sm text-gray-500 dark:text-dark-400">{{ user.username }}</p></div>
      </div>

      <div v-if="loading" class="flex justify-center py-8">
        <svg class="h-8 w-8 animate-spin text-primary-500" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
      </div>

      <!--
        载入失败绝不能落入「未生成」空态：空态紧挨着「生成」按钮，而生成/轮换
        用的是同一个 rotate 接口——一旦管理员在真实令牌仍然有效时误按生成，
        会立刻让该用户依赖此令牌的第三方集成失效。载入失败必须停在一个独立
        的、不带生成/撤销按钮的重试态上（与用户侧 ProfileAccessTokenCard 同理）。
      -->
      <div
        v-else-if="loadFailed"
        data-testid="admin-access-token-load-error"
        class="rounded-lg border border-dashed border-red-200 px-4 py-8 text-center text-sm text-red-600 dark:border-red-900/40 dark:text-red-400"
      >
        <p>{{ t('admin.users.accessToken.loadFailed') }}</p>
        <button type="button" class="btn btn-secondary btn-sm mt-3" data-testid="admin-access-token-retry" @click="retryLoad">
          {{ t('admin.users.accessToken.loadErrorRetry') }}
        </button>
      </div>

      <template v-else>
        <div
          v-if="!token || !token.token"
          data-testid="admin-access-token-empty"
          class="rounded-lg border border-dashed border-gray-200 px-4 py-8 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-gray-400"
        >
          {{ t('admin.users.accessToken.empty') }}
        </div>

        <div v-else class="space-y-3">
          <div class="flex items-center gap-2">
            <code
              data-testid="admin-access-token-value"
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
            {{ t('admin.users.accessToken.createdAt', { date: formatDateTime(token.created_at) }) }}
            <template v-if="token.last_used_at">
              · {{ t('admin.users.accessToken.lastUsed', { date: formatDateTime(token.last_used_at) }) }}
            </template>
          </p>
        </div>

        <div class="flex flex-wrap gap-2">
          <button
            type="button"
            class="btn btn-primary"
            data-testid="admin-access-token-generate"
            :disabled="actionBusy"
            @click="handleRotate"
          >
            {{
              actionBusy
                ? t('common.processing')
                : token && token.token
                  ? t('admin.users.accessToken.regenerate')
                  : t('admin.users.accessToken.generate')
            }}
          </button>
          <button
            v-if="token && token.token"
            type="button"
            class="btn btn-ghost text-red-600 hover:bg-red-50 dark:text-red-300 dark:hover:bg-red-950/30"
            data-testid="admin-access-token-revoke"
            :disabled="actionBusy"
            @click="openRevokeConfirm"
          >
            {{ t('admin.users.accessToken.revoke') }}
          </button>
        </div>
      </template>
    </div>
  </BaseDialog>

  <ConfirmDialog
    :show="showRevokeConfirm"
    :title="t('admin.users.accessToken.revokeTitle')"
    :message="t('admin.users.accessToken.revokeConfirm')"
    :danger="true"
    @confirm="handleRevoke"
    @cancel="showRevokeConfirm = false"
  />

  <!-- 管理员接口挂了 step-up 二次验证：后端返回 STEP_UP_REQUIRED 时弹 TOTP 验证并重试 -->
  <TotpStepUpDialog :controller="stepUp" />
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { useClipboard } from '@/composables/useClipboard'
import { useStepUp, isStepUpBlocked, isStepUpCancelled, stepUpBlockReason } from '@/composables/useStepUp'
import { adminAPI } from '@/api/admin'
import type { UserAccessToken } from '@/api/accessToken'
import type { AdminUser } from '@/types'
import { formatDateTime } from '@/utils/format'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ show: boolean; user: AdminUser | null }>()
const emit = defineEmits(['close', 'success'])
const { t } = useI18n()
const appStore = useAppStore()
const { copyToClipboard } = useClipboard()
const stepUp = useStepUp()

const loading = ref(false)
const loadFailed = ref(false)
const actionBusy = ref(false)
const token = ref<UserAccessToken | null>(null)
const showRevokeConfirm = ref(false)

watch(() => props.show, (v) => {
  if (v && props.user) {
    void load()
  } else {
    // 弹窗关闭：清空明文令牌与所有操作状态，避免下次打开（哪怕换了别的用户）
    // 时先闪现上一个用户的令牌或残留一个已经过期的确认框。
    token.value = null
    loading.value = false
    loadFailed.value = false
    actionBusy.value = false
    showRevokeConfirm.value = false
  }
})

function reportStepUpBlocked(error: unknown): void {
  appStore.showError(
    stepUpBlockReason(error) === 'STEP_UP_ADMIN_API_KEY_FORBIDDEN'
      ? t('stepUp.adminApiKeyForbidden')
      : t('stepUp.notEnabled')
  )
}

async function load(): Promise<void> {
  if (!props.user) return
  const userId = props.user.id
  loading.value = true
  loadFailed.value = false
  try {
    token.value = await stepUp.run(() => adminAPI.accessTokens.getUserAccessToken(userId))
  } catch (error) {
    // 取消二次验证 / 被 step-up 挡下 / 其它任何失败都不能落入「未生成」空态，
    // 一律停在可重试的错误态（理由见上方模板注释）。
    loadFailed.value = true
    if (isStepUpCancelled(error)) {
      // 静默：用户主动取消，不额外弹错误提示
    } else if (isStepUpBlocked(error)) {
      reportStepUpBlocked(error)
    } else {
      appStore.showError((error as { message?: string })?.message || t('admin.users.accessToken.loadFailed'))
    }
  } finally {
    loading.value = false
  }
}

function retryLoad(): void {
  void load()
}

async function handleRotate(): Promise<void> {
  if (!props.user) return
  const userId = props.user.id
  const wasFirstGeneration = !token.value || !token.value.token
  actionBusy.value = true
  try {
    token.value = await stepUp.run(() => adminAPI.accessTokens.rotateUserAccessToken(userId))
    appStore.showSuccess(
      wasFirstGeneration
        ? t('admin.users.accessToken.generateSuccess')
        : t('admin.users.accessToken.rotateSuccess')
    )
    emit('success')
  } catch (error) {
    if (isStepUpCancelled(error)) {
      // 用户主动取消：静默返回，弹窗保持打开，令牌状态不变
    } else if (isStepUpBlocked(error)) {
      reportStepUpBlocked(error)
    } else {
      appStore.showError(
        (error as { message?: string })?.message ||
          t(wasFirstGeneration ? 'admin.users.accessToken.generateFailed' : 'admin.users.accessToken.rotateFailed')
      )
    }
  } finally {
    actionBusy.value = false
  }
}

function openRevokeConfirm(): void {
  showRevokeConfirm.value = true
}

async function handleRevoke(): Promise<void> {
  if (!props.user) return
  const userId = props.user.id
  showRevokeConfirm.value = false
  actionBusy.value = true
  try {
    await stepUp.run(() => adminAPI.accessTokens.revokeUserAccessToken(userId))
    token.value = { token: null, created_at: null, last_used_at: null }
    appStore.showSuccess(t('admin.users.accessToken.revokeSuccess'))
    emit('success')
  } catch (error) {
    if (isStepUpCancelled(error)) {
      // 用户主动取消：静默返回
    } else if (isStepUpBlocked(error)) {
      reportStepUpBlocked(error)
    } else {
      appStore.showError((error as { message?: string })?.message || t('admin.users.accessToken.revokeFailed'))
    }
  } finally {
    actionBusy.value = false
  }
}

async function copyToken(): Promise<void> {
  if (!token.value?.token) return
  await copyToClipboard(token.value.token)
}

function handleClose(): void {
  emit('close')
}

// 仅供测试断言内部状态是否在弹窗关闭时被真正清空——BaseDialog 内部用 v-if
// 控制可见性，DOM 卸载并不代表这个组件实例（以及它持有的明文令牌 ref）被销毁，
// 所以不能只靠"关闭后 DOM 找不到元素了"来证明状态已清空。
defineExpose({ token, loading, loadFailed, actionBusy, showRevokeConfirm })
</script>
