<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { opsAPI } from '@/api/admin/ops'
import type { LarkNotificationConfig, AlertSeverity } from '../types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const config = ref<LarkNotificationConfig | null>(null)
const showEditor = ref(false)
const saving = ref(false)
const testing = ref(false)
const draft = ref<LarkNotificationConfig | null>(null)

const modeOptions = [
  { value: 'webhook', label: t('admin.ops.lark.modeWebhook') },
  { value: 'app', label: t('admin.ops.lark.modeApp') }
]

const receiveIDTypeOptions = [
  { value: 'chat_id', label: 'chat_id' },
  { value: 'open_id', label: 'open_id' },
  { value: 'user_id', label: 'user_id' },
  { value: 'union_id', label: 'union_id' }
]

const severityOptions: Array<{ value: AlertSeverity | ''; label: string }> = [
  { value: '', label: t('admin.ops.email.minSeverityAll') },
  { value: 'critical', label: t('common.critical') },
  { value: 'warning', label: t('common.warning') },
  { value: 'info', label: t('common.info') }
]

async function loadConfig() {
  loading.value = true
  try {
    config.value = await opsAPI.getLarkNotificationConfig()
  } catch (err: any) {
    console.error('[OpsLarkNotificationCard] Failed to load config', err)
    appStore.showError(err?.response?.data?.detail || t('admin.ops.lark.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function saveConfig() {
  if (!draft.value) return
  if (!editorValidation.value.valid) {
    appStore.showError(editorValidation.value.errors[0] || t('admin.ops.lark.validation.invalid'))
    return
  }
  saving.value = true
  try {
    config.value = await opsAPI.updateLarkNotificationConfig(draft.value)
    showEditor.value = false
    appStore.showSuccess(t('admin.ops.lark.saveSuccess'))
  } catch (err: any) {
    console.error('[OpsLarkNotificationCard] Failed to save config', err)
    appStore.showError(err?.response?.data?.detail || t('admin.ops.lark.saveFailed'))
  } finally {
    saving.value = false
  }
}

// testConnection: if called with a config, test against that config (draft preview).
// Otherwise test against the saved DB config.
async function testConnection(cfg?: LarkNotificationConfig | null) {
  testing.value = true
  try {
    await opsAPI.testLarkNotification(cfg ?? undefined)
    appStore.showSuccess(t('admin.ops.lark.testSuccess'))
  } catch (err: any) {
    appStore.showError(err?.response?.data?.detail || err?.message || t('admin.ops.lark.testFailed'))
  } finally {
    testing.value = false
  }
}

function openEditor() {
  if (!config.value) return
  draft.value = JSON.parse(JSON.stringify(config.value))
  showEditor.value = true
}

const editorValidation = computed(() => {
  const errors: string[] = []
  const d = draft.value
  if (!d) return { valid: true, errors }

  if (d.enabled) {
    if (d.mode === 'webhook' && !d.webhook_url.trim()) {
      errors.push(t('admin.ops.lark.validation.webhookRequired'))
    }
    if (d.mode === 'app') {
      if (!d.app_id.trim()) errors.push(t('admin.ops.lark.validation.appIDRequired'))
      if (!d.app_secret.trim()) errors.push(t('admin.ops.lark.validation.appSecretRequired'))
      if (!d.receive_id.trim()) errors.push(t('admin.ops.lark.validation.receiveIDRequired'))
    }
  }

  return { valid: errors.length === 0, errors }
})

const modeLabelMap: Record<string, string> = {
  webhook: 'Webhook',
  app: 'App API'
}

onMounted(() => {
  loadConfig()
})
</script>

<template>
  <div class="rounded-3xl bg-white p-6 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700">
    <div class="mb-4 flex items-start justify-between gap-4">
      <div>
        <h3 class="text-sm font-bold text-gray-900 dark:text-white">{{ t('admin.ops.lark.title') }}</h3>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.lark.description') }}</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          class="flex items-center gap-1.5 rounded-lg bg-gray-100 px-3 py-1.5 text-xs font-bold text-gray-700 transition-colors hover:bg-gray-200 disabled:cursor-not-allowed disabled:opacity-50 dark:bg-dark-700 dark:text-gray-300 dark:hover:bg-dark-600"
          :disabled="loading"
          @click="loadConfig"
        >
          <svg class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
          </svg>
          {{ t('common.refresh') }}
        </button>
        <button
          class="btn btn-sm btn-secondary"
          :disabled="!config || testing"
          @click="() => testConnection()"
        >
          {{ testing ? t('admin.ops.lark.testing') : t('admin.ops.lark.test') }}
        </button>
        <button class="btn btn-sm btn-secondary" :disabled="!config" @click="openEditor">{{ t('common.edit') }}</button>
      </div>
    </div>

    <div v-if="!config" class="text-sm text-gray-500 dark:text-gray-400">
      <span v-if="loading">{{ t('admin.ops.lark.loading') }}</span>
      <span v-else>{{ t('admin.ops.lark.noData') }}</span>
    </div>

    <div v-else class="space-y-4">
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <div class="grid grid-cols-1 gap-3 md:grid-cols-2 lg:grid-cols-3">
          <div class="text-xs text-gray-600 dark:text-gray-300">
            {{ t('common.enabled') }}:
            <span
              class="ml-1 font-medium"
              :class="config.enabled ? 'text-green-600 dark:text-green-400' : 'text-gray-900 dark:text-white'"
            >
              {{ config.enabled ? t('common.enabled') : t('common.disabled') }}
            </span>
          </div>
          <div class="text-xs text-gray-600 dark:text-gray-300">
            {{ t('admin.ops.lark.mode') }}:
            <span class="ml-1 font-medium text-gray-900 dark:text-white">{{ modeLabelMap[config.mode] || config.mode }}</span>
          </div>
          <div v-if="config.mode === 'webhook'" class="text-xs text-gray-600 dark:text-gray-300">
            {{ t('admin.ops.lark.webhookURL') }}:
            <span class="ml-1 font-medium text-gray-900 dark:text-white">
              {{ config.webhook_url ? t('admin.ops.lark.configured') : t('admin.ops.lark.notConfigured') }}
            </span>
          </div>
          <div v-if="config.mode === 'app'" class="text-xs text-gray-600 dark:text-gray-300">
            App ID:
            <span class="ml-1 font-medium text-gray-900 dark:text-white">
              {{ config.app_id ? config.app_id : t('admin.ops.lark.notConfigured') }}
            </span>
          </div>
        </div>
      </div>

      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-2 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.lark.alertTitle') }}</h4>
        <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
          <div class="text-xs text-gray-600 dark:text-gray-300">
            {{ t('common.enabled') }}:
            <span class="ml-1 font-medium text-gray-900 dark:text-white">
              {{ config.alert.enabled ? t('common.enabled') : t('common.disabled') }}
            </span>
          </div>
          <div class="text-xs text-gray-600 dark:text-gray-300">
            {{ t('admin.ops.email.minSeverity') }}:
            <span class="ml-1 font-medium text-gray-900 dark:text-white">
              {{ config.alert.min_severity || t('admin.ops.email.minSeverityAll') }}
            </span>
          </div>
        </div>
      </div>
    </div>
  </div>

  <BaseDialog :show="showEditor" :title="t('admin.ops.lark.title')" width="wide" @close="showEditor = false">
    <div v-if="draft" class="space-y-5">
      <div
        v-if="!editorValidation.valid"
        class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-800 dark:border-amber-900/50 dark:bg-amber-900/20 dark:text-amber-200"
      >
        <div class="font-bold">{{ t('admin.ops.lark.validation.title') }}</div>
        <ul class="mt-1 list-disc space-y-1 pl-4">
          <li v-for="msg in editorValidation.errors" :key="msg">{{ msg }}</li>
        </ul>
      </div>

      <!-- Global toggle -->
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <div class="flex items-center justify-between">
          <span class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.lark.globalEnabled') }}</span>
          <label class="inline-flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
            <input v-model="draft.enabled" type="checkbox" class="h-4 w-4 rounded border-gray-300" />
            <span>{{ draft.enabled ? t('common.enabled') : t('common.disabled') }}</span>
          </label>
        </div>
      </div>

      <!-- Mode selection -->
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.lark.connectionTitle') }}</h4>
        <div class="space-y-4">
          <div>
            <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.ops.lark.mode') }}</div>
            <Select v-model="draft.mode" :options="modeOptions" />
          </div>

          <!-- Webhook mode -->
          <div v-if="draft.mode === 'webhook'">
            <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.ops.lark.webhookURL') }}</div>
            <input
              v-model="draft.webhook_url"
              type="url"
              class="input"
              :placeholder="t('admin.ops.lark.webhookURLPlaceholder')"
            />
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.lark.webhookHint') }}</p>
          </div>

          <!-- App mode -->
          <template v-if="draft.mode === 'app'">
            <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
              <div>
                <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">App ID</div>
                <input v-model="draft.app_id" type="text" class="input" placeholder="cli_xxxxxxxx" />
              </div>
              <div>
                <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">App Secret</div>
                <input v-model="draft.app_secret" type="password" class="input" placeholder="••••••••" />
              </div>
              <div>
                <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.ops.lark.receiveID') }}</div>
                <input v-model="draft.receive_id" type="text" class="input" :placeholder="t('admin.ops.lark.receiveIDPlaceholder')" />
              </div>
              <div>
                <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.ops.lark.receiveIDType') }}</div>
                <Select v-model="draft.receive_id_type" :options="receiveIDTypeOptions" />
              </div>
            </div>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.lark.appHint') }}</p>
          </template>
        </div>
      </div>

      <!-- Alert settings -->
      <div class="rounded-2xl bg-gray-50 p-4 dark:bg-dark-700/50">
        <h4 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.lark.alertTitle') }}</h4>
        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('common.enabled') }}</div>
            <label class="inline-flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <input v-model="draft.alert.enabled" type="checkbox" class="h-4 w-4 rounded border-gray-300" />
              <span>{{ draft.alert.enabled ? t('common.enabled') : t('common.disabled') }}</span>
            </label>
          </div>
          <div>
            <div class="mb-1 text-xs font-medium text-gray-600 dark:text-gray-300">{{ t('admin.ops.email.minSeverity') }}</div>
            <Select v-model="draft.alert.min_severity" :options="severityOptions" />
          </div>
        </div>
        <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.lark.alertHint') }}</p>
      </div>
    </div>

    <template #footer>
      <div class="flex w-full items-center justify-between gap-2">
        <button
          class="btn btn-secondary"
          :disabled="testing || !editorValidation.valid"
          @click="testConnection(draft ?? undefined)"
        >
          {{ testing ? t('admin.ops.lark.testing') : t('admin.ops.lark.testConnectivity') }}
        </button>
        <div class="flex gap-2">
          <button class="btn btn-secondary" @click="showEditor = false">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" :disabled="saving || !editorValidation.valid" @click="saveConfig">
            {{ saving ? t('common.saving') : t('common.save') }}
          </button>
        </div>
      </div>
    </template>
  </BaseDialog>
</template>
