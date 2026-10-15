<template>
  <div class="border-t border-gray-200 pt-4 mt-4 dark:border-dark-400" data-testid="group-model-rate-multipliers">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div class="min-w-0 flex-1">
        <h4 class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.groups.modelRates.title') }}</h4>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.modelRates.description') }}</p>
      </div>
      <button type="button" class="btn btn-secondary shrink-0 whitespace-nowrap" data-testid="group-model-rate-add" @click="addEntry">
        <Icon name="plus" size="sm" class="mr-1" />{{ t('admin.groups.modelRates.add') }}
      </button>
    </div>
    <div v-if="entries.length > 0" class="mt-3 space-y-2">
      <div
        v-for="(entry, index) in entries"
        :key="index"
        class="rounded-lg border border-gray-200 p-3 dark:border-dark-600"
        data-testid="group-model-rate-entry"
      >
        <div class="flex flex-wrap items-start gap-3">
          <div class="min-w-0 flex-1">
            <label class="input-label">{{ t('admin.groups.modelRates.models') }}</label>
            <ModelTagInput
              :models="entry.models"
              :platform="platform"
              :placeholder="t('admin.groups.modelRates.modelsPlaceholder')"
              @update:models="updateEntry(index, { models: $event })"
            />
          </div>
          <div class="w-36">
            <label class="input-label">{{ t('admin.groups.modelRates.rate') }}</label>
            <input
              :value="entry.rate_multiplier ?? ''"
              type="number"
              step="0.001"
              min="0.001"
              class="input"
              :placeholder="String(groupRate ?? 1)"
              data-testid="group-model-rate-value"
              @input="updateEntry(index, { rate_multiplier: ($event.target as HTMLInputElement).value })"
            />
            <p v-if="hintFor(entry)" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ hintFor(entry) }}</p>
          </div>
          <button
            type="button"
            class="mt-6 rounded p-1.5 text-gray-400 hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-900/20"
            :title="t('common.delete')"
            data-testid="group-model-rate-remove"
            @click="removeEntry(index)"
          >
            <Icon name="trash" size="sm" />
          </button>
        </div>
      </div>
    </div>
    <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.modelRates.precedenceHint') }}</p>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import ModelTagInput from '@/components/admin/channel/ModelTagInput.vue'
import { emptyModelRateEntry, type ModelRateFormEntry } from '@/views/admin/groupsModelRateMultipliers'

const props = defineProps<{
  entries: ModelRateFormEntry[]
  platform?: string
  groupRate?: number | string | null
}>()

const emit = defineEmits<{
  'update:entries': [entries: ModelRateFormEntry[]]
}>()

const { t } = useI18n()

function addEntry() {
  emit('update:entries', [...props.entries, emptyModelRateEntry()])
}

function removeEntry(index: number) {
  emit('update:entries', props.entries.filter((_, i) => i !== index))
}

function updateEntry(index: number, patch: Partial<ModelRateFormEntry>) {
  emit('update:entries', props.entries.map((entry, i) => (i === index ? { ...entry, ...patch } : entry)))
}

// 与分组默认倍率对比，提示这是加价还是折扣。
function hintFor(entry: ModelRateFormEntry): string {
  const rate = Number(entry.rate_multiplier)
  const base = Number(props.groupRate)
  if (entry.rate_multiplier === null || entry.rate_multiplier === '' || !Number.isFinite(rate) || rate <= 0 || !Number.isFinite(base) || base <= 0) {
    return ''
  }
  if (rate === base) {
    return t('admin.groups.modelRates.sameAsGroup')
  }
  return t('admin.groups.modelRates.vsGroup', { percent: Math.round((rate / base) * 1000) / 10 })
}
</script>
