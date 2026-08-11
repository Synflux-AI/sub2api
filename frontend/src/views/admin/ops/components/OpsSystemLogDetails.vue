<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OpsSystemLog } from '@/api/admin/ops'
import { useClipboard } from '@/composables/useClipboard'

const props = defineProps<{
  log: OpsSystemLog
}>()

const { t } = useI18n()
const { copyToClipboard } = useClipboard()

const isNonEmptyBaseValue = (value: unknown) => {
  if (value == null) return false
  return typeof value !== 'string' || value.trim() !== ''
}

const baseFields = computed(() => Object.entries(props.log).filter(([key, value]) => (
  key !== 'extra' && isNonEmptyBaseValue(value)
)))

const extraFields = computed(() => Object.entries(props.log.extra || {}))

const formatDetailValue = (value: unknown) => {
  if (value === null) return 'null'
  if (value === undefined) return 'undefined'
  if (typeof value === 'string') return value === '' ? '""' : value
  if (typeof value === 'object') {
    try {
      return JSON.stringify(value, null, 2)
    } catch {
      return String(value)
    }
  }
  return String(value)
}

const copyLogJSON = async () => {
  await copyToClipboard(
    JSON.stringify(props.log, null, 2),
    t('admin.ops.systemLogs.copySuccess')
  )
}
</script>

<template>
  <section class="bg-gray-50 px-3 py-3 dark:bg-dark-800/60" data-testid="system-log-details">
    <div class="mb-3 flex items-center justify-between gap-3">
      <h4 class="text-xs font-semibold text-gray-700 dark:text-gray-200">
        {{ t('admin.ops.systemLogs.fullDetails') }}
      </h4>
      <button
        type="button"
        class="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md text-gray-500 hover:bg-gray-200 hover:text-gray-800 focus:outline-none focus:ring-2 focus:ring-blue-500 dark:text-gray-400 dark:hover:bg-dark-700 dark:hover:text-white"
        :aria-label="t('admin.ops.systemLogs.copyJson')"
        :title="t('admin.ops.systemLogs.copyJson')"
        data-testid="copy-system-log-json"
        @click="copyLogJSON"
      >
        <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <rect width="14" height="14" x="8" y="8" rx="2" ry="2" />
          <path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2" />
        </svg>
      </button>
    </div>

    <div class="grid gap-x-6 gap-y-4 lg:grid-cols-2">
      <div class="min-w-0">
        <h5 class="mb-2 text-[11px] font-semibold uppercase text-gray-500 dark:text-gray-400">
          {{ t('admin.ops.systemLogs.baseFields') }}
        </h5>
        <dl class="divide-y divide-gray-200 dark:divide-dark-700">
          <div v-for="[key, value] in baseFields" :key="key" class="grid min-w-0 gap-1 py-2 sm:grid-cols-[150px_minmax(0,1fr)]">
            <dt class="break-all font-mono text-[11px] font-medium text-gray-500 dark:text-gray-400">{{ key }}</dt>
            <dd class="min-w-0">
              <pre class="whitespace-pre-wrap break-all font-mono text-xs text-gray-800 dark:text-gray-200">{{ formatDetailValue(value) }}</pre>
            </dd>
          </div>
        </dl>
      </div>

      <div v-if="extraFields.length > 0" class="min-w-0">
        <h5 class="mb-2 text-[11px] font-semibold uppercase text-gray-500 dark:text-gray-400">
          {{ t('admin.ops.systemLogs.extraFields') }}
        </h5>
        <dl class="divide-y divide-gray-200 dark:divide-dark-700">
          <div v-for="[key, value] in extraFields" :key="key" class="grid min-w-0 gap-1 py-2 sm:grid-cols-[150px_minmax(0,1fr)]">
            <dt class="break-all font-mono text-[11px] font-medium text-gray-500 dark:text-gray-400">{{ key }}</dt>
            <dd class="min-w-0">
              <pre class="whitespace-pre-wrap break-all font-mono text-xs text-gray-800 dark:text-gray-200">{{ formatDetailValue(value) }}</pre>
            </dd>
          </div>
        </dl>
      </div>
    </div>
  </section>
</template>
