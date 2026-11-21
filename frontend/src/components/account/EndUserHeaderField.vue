<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import {
  DEFAULT_END_USER_HEADER_NAME,
  emptyEndUserHeaderState,
  type EndUserHeaderState
} from './endUserHeader'

defineProps<{ idPrefix: string }>()
const model = defineModel<EndUserHeaderState>({ default: emptyEndUserHeaderState })
const { t } = useI18n()

const update = (patch: Partial<EndUserHeaderState>) => {
  model.value = { ...model.value, ...patch }
}
</script>

<template>
  <div class="border-t border-gray-200 pt-4 dark:border-dark-600">
    <div class="flex items-center justify-between">
      <div>
        <label class="input-label mb-0">{{ t('admin.accounts.endUserHeader.label') }}</label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.endUserHeader.hint') }}
        </p>
      </div>
      <button
        :id="`${idPrefix}-end-user-header-toggle`"
        type="button"
        @click="update({ enabled: !model.enabled })"
        :class="[
          'relative inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2',
          model.enabled ? 'bg-primary-600' : 'bg-gray-200 dark:bg-dark-600'
        ]"
        :aria-pressed="model.enabled"
      >
        <span
          :class="[
            'pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out',
            model.enabled ? 'translate-x-5' : 'translate-x-0'
          ]"
        />
      </button>
    </div>
    <div v-if="model.enabled" class="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
      <div>
        <label class="input-label">{{ t('admin.accounts.endUserHeader.name') }}</label>
        <input
          :value="model.name"
          :data-testid="`${idPrefix}-end-user-header-name`"
          type="text"
          class="input"
          :placeholder="DEFAULT_END_USER_HEADER_NAME"
          @input="update({ name: ($event.target as HTMLInputElement).value })"
        />
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.endUserHeader.nameHint') }}</p>
      </div>
      <div>
        <label class="input-label">{{ t('admin.accounts.endUserHeader.prefix') }}</label>
        <input
          :value="model.prefix"
          :data-testid="`${idPrefix}-end-user-header-prefix`"
          type="text"
          class="input"
          maxlength="32"
          :placeholder="t('admin.accounts.endUserHeader.prefixPlaceholder')"
          @input="update({ prefix: ($event.target as HTMLInputElement).value })"
        />
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.endUserHeader.prefixHint') }}</p>
      </div>
    </div>
  </div>
</template>
