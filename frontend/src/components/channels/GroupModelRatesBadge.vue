<template>
  <span
    v-if="items.length > 0"
    class="inline-flex items-center gap-1 rounded-md bg-primary-50 px-1.5 py-0.5 text-[10px] font-medium text-primary-700 dark:bg-primary-900/20 dark:text-primary-300"
    :title="title"
    data-testid="group-model-rates-badge"
  >
    <Icon name="sparkles" size="xs" class="h-3 w-3" />
    {{ t('availableChannels.modelRates.badge', { count: items.length }) }}
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import type { GroupModelRateMultiplier } from '@/types'

// 分组单独设置了倍率的模型：展示该用户的生效值（用户改过的优先，否则继承分组），
// 与计费同一口径。命中这些模型时替换分组/专属倍率，不相乘。
const props = defineProps<{
  rules?: GroupModelRateMultiplier[] | null
  userModelRates?: Record<string, number> | null
}>()

const { t } = useI18n()

const items = computed(() =>
  (props.rules || []).flatMap((rule) =>
    (rule.models || []).map((pattern) => ({
      pattern,
      rate: props.userModelRates?.[pattern] ?? rule.rate_multiplier
    }))
  )
)

const title = computed(() =>
  [t('availableChannels.modelRates.title'), ...items.value.map((item) => `${item.pattern}: ${item.rate}x`)].join('\n')
)
</script>
