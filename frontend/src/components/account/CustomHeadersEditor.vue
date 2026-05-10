<template>
  <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
    <div class="flex items-center justify-between">
      <div>
        <label class="input-label mb-0">
          {{ t('admin.accounts.customHeaders.label') }}
        </label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.customHeaders.hint') }}
        </p>
      </div>
      <button
        type="button"
        @click="toggleEnabled"
        :class="[
          'relative inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2',
          enabled ? 'bg-primary-600' : 'bg-gray-200 dark:bg-dark-600'
        ]"
        :aria-pressed="enabled"
      >
        <span
          :class="[
            'pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out',
            enabled ? 'translate-x-5' : 'translate-x-0'
          ]"
        />
      </button>
    </div>

    <div v-if="enabled" class="mt-3 space-y-2">
      <div
        v-for="(row, index) in rows"
        :key="index"
        class="flex items-start gap-2"
      >
        <input
          v-model="row.key"
          type="text"
          class="input flex-1 font-mono text-sm"
          :placeholder="t('admin.accounts.customHeaders.keyPlaceholder')"
          :class="{ 'border-amber-400': isProtected(row.key) }"
          @input="emitChange"
        />
        <input
          v-model="row.value"
          type="text"
          class="input flex-1 font-mono text-sm"
          :placeholder="t('admin.accounts.customHeaders.valuePlaceholder')"
          @input="emitChange"
        />
        <button
          type="button"
          class="rounded p-2 text-gray-500 hover:bg-gray-100 hover:text-red-500 dark:text-gray-400 dark:hover:bg-dark-700"
          :title="t('common.delete')"
          @click="removeRow(index)"
        >
          <svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              stroke-width="2"
              d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6M1 7h22M9 7V4a1 1 0 011-1h4a1 1 0 011 1v3"
            />
          </svg>
        </button>
      </div>

      <button
        type="button"
        class="text-xs font-medium text-primary-600 hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300"
        @click="addRow"
      >
        + {{ t('admin.accounts.customHeaders.addRow') }}
      </button>

      <p
        v-if="hasProtectedKeys"
        class="mt-2 text-xs text-amber-600 dark:text-amber-400"
      >
        {{ t('admin.accounts.customHeaders.protectedWarning') }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

interface HeaderRow {
  key: string
  value: string
}

interface Props {
  enabled: boolean
  headers: Record<string, string> | null | undefined
}

const props = defineProps<Props>()
const emit = defineEmits<{
  (e: 'update:enabled', value: boolean): void
  (e: 'update:headers', value: Record<string, string>): void
}>()

const rows = ref<HeaderRow[]>([])

const protectedHeaderNames = new Set([
  'host',
  'content-length',
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'trailers',
  'transfer-encoding',
  'upgrade'
])

const isProtected = (key: string) => {
  const trimmed = key.trim().toLowerCase()
  return trimmed.length > 0 && protectedHeaderNames.has(trimmed)
}

const hasProtectedKeys = ref(false)

const recomputeProtected = () => {
  hasProtectedKeys.value = rows.value.some((row) => isProtected(row.key))
}

const syncRowsFromProps = () => {
  const incoming = props.headers || {}
  const next: HeaderRow[] = Object.entries(incoming).map(([k, v]) => ({ key: k, value: v }))
  rows.value = next
  recomputeProtected()
}

watch(
  () => props.headers,
  () => syncRowsFromProps(),
  { immediate: true, deep: true }
)

const toggleEnabled = () => {
  emit('update:enabled', !props.enabled)
}

const addRow = () => {
  rows.value.push({ key: '', value: '' })
}

const removeRow = (index: number) => {
  rows.value.splice(index, 1)
  emitChange()
}

const emitChange = () => {
  recomputeProtected()
  const map: Record<string, string> = {}
  for (const row of rows.value) {
    const key = row.key.trim()
    if (!key) {
      continue
    }
    map[key] = row.value
  }
  emit('update:headers', map)
}
</script>
