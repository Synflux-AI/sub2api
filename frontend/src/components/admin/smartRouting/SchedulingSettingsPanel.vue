<template>
  <div class="space-y-6">
    <div class="flex flex-wrap items-center gap-2">
      <p class="min-w-0 flex-1 text-xs text-gray-500 dark:text-dark-400">
        {{ t('admin.smartRouting.settings.configOnlyNotice') }}
      </p>
      <button class="btn btn-secondary btn-sm" :disabled="loading" @click="reload">
        {{ t('admin.smartRouting.settings.reload') }}
      </button>
      <button class="btn btn-primary btn-sm" :disabled="saving || loading" @click="save">
        {{ saving ? t('admin.smartRouting.settings.saving') : t('admin.smartRouting.settings.save') }}
      </button>
    </div>

    <!-- 账号健康分 -->
    <section class="card p-5">
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t('admin.smartRouting.settings.healthTitle') }}
      </h3>
      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
        {{ t('admin.smartRouting.settings.healthHint') }}
      </p>

      <div class="mt-4 space-y-4">
        <div class="flex items-start justify-between gap-4">
          <div class="min-w-0">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.settings.healthScoringEnabled') }}
            </div>
            <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
              {{ t('admin.smartRouting.settings.healthScoringEnabledHint') }}
            </p>
          </div>
          <Toggle v-model="form.scheduling_health_scoring_enabled" />
        </div>

        <div v-if="form.scheduling_health_scoring_enabled" class="flex items-start justify-between gap-4">
          <div class="min-w-0">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.settings.healthShadowMode') }}
            </div>
            <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
              {{ t('admin.smartRouting.settings.healthShadowModeHint') }}
            </p>
          </div>
          <Toggle v-model="form.scheduling_health_shadow_mode" />
        </div>

        <div v-if="form.scheduling_health_scoring_enabled" class="flex items-start justify-between gap-4">
          <div class="min-w-0">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.settings.healthStickyBreak') }}
            </div>
            <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
              {{ t('admin.smartRouting.settings.healthStickyBreakHint') }}
            </p>
          </div>
          <Toggle v-model="form.scheduling_health_sticky_break_enabled" />
        </div>
      </div>
    </section>

    <!-- 价格感知 -->
    <section class="card p-5">
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t('admin.smartRouting.settings.priceTitle') }}
      </h3>
      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
        {{ t('admin.smartRouting.settings.priceHint') }}
      </p>

      <div class="mt-4 flex items-start justify-between gap-4">
        <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.smartRouting.settings.priceAwareEnabled') }}
        </div>
        <Toggle v-model="form.scheduling_price_aware_enabled" />
      </div>
    </section>

    <!-- OpenAI 高级调度权重 -->
    <section class="card p-5">
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t('admin.smartRouting.settings.openaiTitle') }}
      </h3>
      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
        {{ t('admin.smartRouting.settings.openaiHint') }}
      </p>

      <div class="mt-4 space-y-4">
        <div class="flex items-start justify-between gap-4">
          <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.smartRouting.settings.openaiAdvancedEnabled') }}
          </div>
          <Toggle v-model="form.openai_advanced_scheduler_enabled" />
        </div>

        <template v-if="form.openai_advanced_scheduler_enabled">
          <div class="flex items-start justify-between gap-4">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.settings.openaiStickyWeighted') }}
            </div>
            <Toggle v-model="form.openai_advanced_scheduler_sticky_weighted_enabled" />
          </div>

          <div class="flex items-start justify-between gap-4">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.settings.openaiSubscriptionPriority') }}
            </div>
            <Toggle v-model="form.openai_advanced_scheduler_subscription_priority_enabled" />
          </div>

          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <div v-for="field in weightFields" :key="field.key">
              <label class="input-label">{{ field.label }}</label>
              <input v-model="form[field.key]" type="text" inputmode="decimal" class="input" />
              <p v-if="effectiveOf(field.effectiveKey)" class="input-hint">
                {{ t('admin.smartRouting.settings.effectiveValue') }}: {{ effectiveOf(field.effectiveKey) }}
              </p>
            </div>
          </div>
        </template>
      </div>
    </section>

    <!-- 冷却与闸门 -->
    <section class="card p-5">
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t('admin.smartRouting.settings.cooldownTitle') }}
      </h3>
      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
        {{ t('admin.smartRouting.settings.cooldownHint') }}
      </p>

      <div class="mt-4 space-y-3">
        <!-- 这几组是独立的 JSON setting，各有自己的读写接口和校验规则。
             第一版只做「指路」，把编辑入口留在系统设置里，避免在此复制一份表单逻辑后两处漂移。 -->
        <div
          v-for="item in cooldownLinks"
          :key="item.key"
          class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-gray-200 px-4 py-3 dark:border-dark-700"
        >
          <div class="min-w-0">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ item.label }}</div>
            <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
              {{ t('admin.smartRouting.settings.configuredIn') }}
            </p>
          </div>
          <RouterLink :to="item.to" class="btn btn-secondary btn-sm shrink-0">
            {{ item.actionLabel }}
          </RouterLink>
        </div>
      </div>
    </section>

    <!-- 其他 -->
    <section class="card p-5">
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t('admin.smartRouting.settings.otherTitle') }}
      </h3>

      <div class="mt-4 space-y-4">
        <div class="flex items-start justify-between gap-4">
          <div class="min-w-0">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.settings.allowUngroupedKey') }}
            </div>
            <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
              {{ t('admin.smartRouting.settings.allowUngroupedKeyHint') }}
            </p>
          </div>
          <Toggle v-model="form.allow_ungrouped_key_scheduling" />
        </div>

        <div class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-gray-200 px-4 py-3 dark:border-dark-700">
          <div class="min-w-0">
            <div class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.settings.errorHandlingRules') }}
            </div>
            <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
              {{ t('admin.smartRouting.settings.errorHandlingRulesHint') }}
            </p>
          </div>
          <RouterLink to="/admin/error-handling-rules" class="btn btn-secondary btn-sm shrink-0">
            {{ t('admin.smartRouting.settings.openErrorRules') }}
          </RouterLink>
        </div>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink } from 'vue-router'
import { adminAPI } from '@/api/admin'
import type { SystemSettings, UpdateSettingsRequest } from '@/api/admin/settings'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import Toggle from '@/components/common/Toggle.vue'

const emit = defineEmits<{ (e: 'saved', settings: SystemSettings): void }>()

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const saving = ref(false)
const loaded = ref<SystemSettings | null>(null)

/** 权重字段在后端以字符串保存（允许空值表示"用默认"），这里保持字符串不做数值转换。 */
type WeightKey =
  | 'openai_advanced_scheduler_lb_top_k'
  | 'openai_advanced_scheduler_weight_priority'
  | 'openai_advanced_scheduler_weight_load'
  | 'openai_advanced_scheduler_weight_queue'
  | 'openai_advanced_scheduler_weight_error_rate'
  | 'openai_advanced_scheduler_weight_ttft'
  | 'openai_advanced_scheduler_weight_reset'
  | 'openai_advanced_scheduler_weight_quota_headroom'
  | 'openai_advanced_scheduler_weight_upstream_cost'
  | 'openai_advanced_scheduler_weight_previous_response'
  | 'openai_advanced_scheduler_weight_session_sticky'

const form = reactive<
  {
    scheduling_health_scoring_enabled: boolean
    scheduling_health_shadow_mode: boolean
    scheduling_health_sticky_break_enabled: boolean
    scheduling_price_aware_enabled: boolean
    allow_ungrouped_key_scheduling: boolean
    openai_advanced_scheduler_enabled: boolean
    openai_advanced_scheduler_sticky_weighted_enabled: boolean
    openai_advanced_scheduler_subscription_priority_enabled: boolean
  } & Record<WeightKey, string>
>({
  scheduling_health_scoring_enabled: false,
  scheduling_health_shadow_mode: true,
  scheduling_health_sticky_break_enabled: true,
  scheduling_price_aware_enabled: false,
  allow_ungrouped_key_scheduling: false,
  openai_advanced_scheduler_enabled: false,
  openai_advanced_scheduler_sticky_weighted_enabled: false,
  openai_advanced_scheduler_subscription_priority_enabled: false,
  openai_advanced_scheduler_lb_top_k: '',
  openai_advanced_scheduler_weight_priority: '',
  openai_advanced_scheduler_weight_load: '',
  openai_advanced_scheduler_weight_queue: '',
  openai_advanced_scheduler_weight_error_rate: '',
  openai_advanced_scheduler_weight_ttft: '',
  openai_advanced_scheduler_weight_reset: '',
  openai_advanced_scheduler_weight_quota_headroom: '',
  openai_advanced_scheduler_weight_upstream_cost: '',
  openai_advanced_scheduler_weight_previous_response: '',
  openai_advanced_scheduler_weight_session_sticky: ''
})

const weightFields = computed<
  { key: WeightKey; label: string; effectiveKey: keyof SystemSettings }[]
>(() => [
  {
    key: 'openai_advanced_scheduler_lb_top_k',
    label: t('admin.smartRouting.settings.openaiLbTopK'),
    effectiveKey: 'openai_advanced_scheduler_effective_lb_top_k'
  },
  {
    key: 'openai_advanced_scheduler_weight_priority',
    label: t('admin.smartRouting.settings.weightPriority'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_priority'
  },
  {
    key: 'openai_advanced_scheduler_weight_load',
    label: t('admin.smartRouting.settings.weightLoad'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_load'
  },
  {
    key: 'openai_advanced_scheduler_weight_queue',
    label: t('admin.smartRouting.settings.weightQueue'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_queue'
  },
  {
    key: 'openai_advanced_scheduler_weight_error_rate',
    label: t('admin.smartRouting.settings.weightErrorRate'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_error_rate'
  },
  {
    key: 'openai_advanced_scheduler_weight_ttft',
    label: t('admin.smartRouting.settings.weightTtft'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_ttft'
  },
  {
    key: 'openai_advanced_scheduler_weight_reset',
    label: t('admin.smartRouting.settings.weightReset'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_reset'
  },
  {
    key: 'openai_advanced_scheduler_weight_quota_headroom',
    label: t('admin.smartRouting.settings.weightQuotaHeadroom'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_quota_headroom'
  },
  {
    key: 'openai_advanced_scheduler_weight_upstream_cost',
    label: t('admin.smartRouting.settings.weightUpstreamCost'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_upstream_cost'
  },
  {
    key: 'openai_advanced_scheduler_weight_previous_response',
    label: t('admin.smartRouting.settings.weightPreviousResponse'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_previous_response'
  },
  {
    key: 'openai_advanced_scheduler_weight_session_sticky',
    label: t('admin.smartRouting.settings.weightSessionSticky'),
    effectiveKey: 'openai_advanced_scheduler_effective_weight_session_sticky'
  }
])

const cooldownLinks = computed(() => [
  {
    key: '429',
    label: t('admin.smartRouting.settings.cooldown429'),
    to: '/admin/settings',
    actionLabel: t('admin.smartRouting.settings.openInSettings')
  },
  {
    key: '529',
    label: t('admin.smartRouting.settings.cooldown529'),
    to: '/admin/settings',
    actionLabel: t('admin.smartRouting.settings.openInSettings')
  },
  {
    key: 'streamTimeout',
    label: t('admin.smartRouting.settings.streamTimeout'),
    to: '/admin/settings',
    actionLabel: t('admin.smartRouting.settings.openInSettings')
  },
  {
    key: 'tempUnschedGuard',
    label: t('admin.smartRouting.settings.tempUnschedGuard'),
    to: '/admin/settings',
    actionLabel: t('admin.smartRouting.settings.openInSettings')
  }
])

function effectiveOf(key: keyof SystemSettings): string {
  const value = loaded.value?.[key]
  return value == null || value === '' ? '' : String(value)
}

function applySettings(settings: SystemSettings) {
  loaded.value = settings
  form.scheduling_health_scoring_enabled = !!settings.scheduling_health_scoring_enabled
  form.scheduling_health_shadow_mode = !!settings.scheduling_health_shadow_mode
  form.scheduling_health_sticky_break_enabled = !!settings.scheduling_health_sticky_break_enabled
  form.scheduling_price_aware_enabled = !!settings.scheduling_price_aware_enabled
  form.allow_ungrouped_key_scheduling = !!settings.allow_ungrouped_key_scheduling
  form.openai_advanced_scheduler_enabled = !!settings.openai_advanced_scheduler_enabled
  form.openai_advanced_scheduler_sticky_weighted_enabled =
    !!settings.openai_advanced_scheduler_sticky_weighted_enabled
  form.openai_advanced_scheduler_subscription_priority_enabled =
    !!settings.openai_advanced_scheduler_subscription_priority_enabled
  for (const field of weightFields.value) {
    form[field.key] = (settings[field.key] as string | undefined) ?? ''
  }
}

async function reload() {
  loading.value = true
  try {
    applySettings(await adminAPI.settings.getSettings())
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.smartRouting.settings.saveFailed')))
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    // 只提交本页管理的键，避免把整份 settings 回写覆盖掉其他页面的并发修改。
    const payload: UpdateSettingsRequest = {
      scheduling_health_scoring_enabled: form.scheduling_health_scoring_enabled,
      scheduling_health_shadow_mode: form.scheduling_health_shadow_mode,
      scheduling_health_sticky_break_enabled: form.scheduling_health_sticky_break_enabled,
      scheduling_price_aware_enabled: form.scheduling_price_aware_enabled,
      allow_ungrouped_key_scheduling: form.allow_ungrouped_key_scheduling,
      openai_advanced_scheduler_enabled: form.openai_advanced_scheduler_enabled,
      openai_advanced_scheduler_sticky_weighted_enabled:
        form.openai_advanced_scheduler_sticky_weighted_enabled,
      openai_advanced_scheduler_subscription_priority_enabled:
        form.openai_advanced_scheduler_subscription_priority_enabled
    }
    const payloadRecord = payload as Record<string, unknown>
    for (const field of weightFields.value) {
      payloadRecord[field.key] = form[field.key]
    }

    const updated = await adminAPI.settings.updateSettings(payload)
    applySettings(updated)
    appStore.showSuccess(t('admin.smartRouting.settings.savedSuccess'))
    emit('saved', updated)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.smartRouting.settings.saveFailed')))
  } finally {
    saving.value = false
  }
}

onMounted(reload)

defineExpose({ reload })
</script>
