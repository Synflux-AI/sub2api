<template>
  <div class="space-y-6">
    <!-- 调度开关状态灯 -->
    <section>
      <div class="mb-2 flex flex-wrap items-baseline gap-2">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.smartRouting.overview.switchesTitle') }}
        </h3>
        <p class="text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.smartRouting.overview.switchesHint') }}
        </p>
      </div>

      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <div class="card p-4">
          <div class="flex items-center justify-between gap-2">
            <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.overview.healthScoring') }}
            </span>
            <span :class="['badge', healthModeBadgeClass]">{{ healthModeLabel }}</span>
          </div>
          <p class="mt-1.5 text-xs text-gray-500 dark:text-dark-400">{{ healthModeHint }}</p>
        </div>

        <div class="card p-4">
          <div class="flex items-center justify-between gap-2">
            <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.overview.stickyBreak') }}
            </span>
            <span :class="['badge', settings?.scheduling_health_sticky_break_enabled ? 'badge-success' : 'badge-gray']">
              {{ settings?.scheduling_health_sticky_break_enabled ? t('admin.smartRouting.overview.enabled') : t('admin.smartRouting.overview.disabled') }}
            </span>
          </div>
          <p class="mt-1.5 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.smartRouting.overview.stickyBreakHint') }}
          </p>
        </div>

        <div class="card p-4">
          <div class="flex items-center justify-between gap-2">
            <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.overview.priceAware') }}
            </span>
            <span :class="['badge', settings?.scheduling_price_aware_enabled ? 'badge-success' : 'badge-gray']">
              {{ settings?.scheduling_price_aware_enabled ? t('admin.smartRouting.overview.enabled') : t('admin.smartRouting.overview.disabled') }}
            </span>
          </div>
          <p class="mt-1.5 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.smartRouting.overview.priceAwareHint') }}
          </p>
        </div>

        <div class="card p-4">
          <div class="flex items-center justify-between gap-2">
            <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.smartRouting.overview.openaiAdvanced') }}
            </span>
            <span :class="['badge', settings?.openai_advanced_scheduler_enabled ? 'badge-success' : 'badge-gray']">
              {{ settings?.openai_advanced_scheduler_enabled ? t('admin.smartRouting.overview.enabled') : t('admin.smartRouting.overview.disabled') }}
            </span>
          </div>
          <p class="mt-1.5 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.smartRouting.overview.openaiAdvancedHint') }}
          </p>
        </div>
      </div>
    </section>

    <!-- 账号可调度性统计 -->
    <section>
      <div class="mb-2 flex flex-wrap items-baseline gap-2">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.smartRouting.overview.countsTitle') }}
        </h3>
        <p class="text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.smartRouting.overview.countsHint') }}
        </p>
      </div>

      <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-7">
        <div v-for="stat in gateStats" :key="stat.key" class="card p-4">
          <div class="text-xs text-gray-500 dark:text-dark-400">{{ stat.label }}</div>
          <div :class="['mt-1 text-2xl font-bold', stat.class]">{{ stat.value }}</div>
        </div>
      </div>
    </section>

    <!-- 平台 / 分组分布 -->
    <section class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <div class="card overflow-hidden">
        <div class="border-b border-gray-200 px-4 py-3 dark:border-dark-700">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
            {{ t('admin.smartRouting.overview.byPlatformTitle') }}
          </h3>
        </div>
        <table class="w-full text-sm">
          <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800 dark:text-dark-400">
            <tr>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.platform') }}</th>
              <th class="px-4 py-2 text-right font-medium">{{ t('admin.smartRouting.overview.available') }}</th>
              <th class="px-4 py-2 text-right font-medium">{{ t('admin.smartRouting.overview.total') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="row in byPlatform"
              :key="row.key"
              class="border-t border-gray-100 dark:border-dark-700/50"
            >
              <td class="px-4 py-2 text-gray-900 dark:text-white">{{ row.key }}</td>
              <td class="px-4 py-2 text-right" :class="row.available < row.total ? 'text-amber-600 dark:text-amber-400' : 'text-gray-600 dark:text-gray-300'">
                {{ row.available }}
              </td>
              <td class="px-4 py-2 text-right text-gray-600 dark:text-gray-300">{{ row.total }}</td>
            </tr>
            <tr v-if="byPlatform.length === 0">
              <td colspan="3" class="px-4 py-6 text-center text-gray-400">{{ t('admin.smartRouting.common.none') }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="card overflow-hidden">
        <div class="border-b border-gray-200 px-4 py-3 dark:border-dark-700">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
            {{ t('admin.smartRouting.overview.byGroupTitle') }}
          </h3>
        </div>
        <div class="max-h-72 overflow-y-auto">
          <table class="w-full text-sm">
            <thead class="sticky top-0 bg-gray-50 text-xs text-gray-500 dark:bg-dark-800 dark:text-dark-400">
              <tr>
                <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.group') }}</th>
                <th class="px-4 py-2 text-right font-medium">{{ t('admin.smartRouting.overview.available') }}</th>
                <th class="px-4 py-2 text-right font-medium">{{ t('admin.smartRouting.overview.total') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="row in byGroup"
                :key="row.key"
                class="border-t border-gray-100 dark:border-dark-700/50"
              >
                <td class="px-4 py-2 text-gray-900 dark:text-white">{{ row.key }}</td>
                <td class="px-4 py-2 text-right" :class="row.available < row.total ? 'text-amber-600 dark:text-amber-400' : 'text-gray-600 dark:text-gray-300'">
                  {{ row.available }}
                </td>
                <td class="px-4 py-2 text-right text-gray-600 dark:text-gray-300">{{ row.total }}</td>
              </tr>
              <tr v-if="byGroup.length === 0">
                <td colspan="3" class="px-4 py-6 text-center text-gray-400">{{ t('admin.smartRouting.common.none') }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </section>

    <!-- 变慢但没报错的账号 -->
    <section v-if="slowAccounts.length > 0 || ttftMonitored">
      <div class="mb-2 flex flex-wrap items-baseline gap-2">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.smartRouting.overview.slowTitle') }}
        </h3>
        <p class="text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.smartRouting.overview.slowHint') }}
        </p>
      </div>

      <div class="card overflow-hidden">
        <table class="w-full text-sm">
          <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800 dark:text-dark-400">
            <tr>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.account') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.platform') }}</th>
              <th class="px-4 py-2 text-right font-medium">{{ t('admin.smartRouting.overview.ttftP50') }}</th>
              <th class="px-4 py-2 text-right font-medium">{{ t('admin.smartRouting.overview.ttftBaseline') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.ttftRatio') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.ttftWorstModel') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="row in slowAccounts"
              :key="row.account.id"
              class="border-t border-gray-100 dark:border-dark-700/50"
            >
              <td class="px-4 py-2">
                <div class="font-medium text-gray-900 dark:text-white">{{ row.account.name }}</div>
                <div class="text-xs text-gray-400">#{{ row.account.id }}</div>
              </td>
              <td class="px-4 py-2 text-gray-600 dark:text-gray-300">{{ row.account.platform }}</td>
              <td class="px-4 py-2 text-right font-mono text-gray-700 dark:text-gray-300">
                {{ formatMs(row.ttft.p50_ms) }}
              </td>
              <td class="px-4 py-2 text-right font-mono text-gray-500 dark:text-dark-400">
                {{ formatMs(row.baselineMs) }}
              </td>
              <td class="px-4 py-2">
                <span :class="['badge text-xs', ttftRatioBadgeClass(row.ttft)]">
                  {{ row.ttft.ratio.toFixed(1) }}×
                </span>
                <span v-if="row.ttft.degraded" class="ml-1.5 text-xs text-red-600 dark:text-red-400">
                  {{ ttftDegradeEnabled
                    ? t('admin.smartRouting.overview.ttftPenalized')
                    : t('admin.smartRouting.overview.ttftWouldPenalize') }}
                </span>
              </td>
              <td class="px-4 py-2 text-xs text-gray-500 dark:text-dark-400">
                <template v-if="row.ttft.worst_model">
                  {{ row.ttft.worst_model }} · {{ row.ttft.worst_ratio.toFixed(1) }}×
                </template>
                <template v-else>—</template>
              </td>
            </tr>
            <tr v-if="slowAccounts.length === 0">
              <td colspan="6" class="px-4 py-8 text-center text-gray-400">
                {{ t('admin.smartRouting.overview.slowEmpty') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 当前不可调度的账号 -->
    <section>
      <div class="mb-2 flex flex-wrap items-baseline gap-2">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.smartRouting.overview.blockedTitle') }}
        </h3>
        <p class="text-xs text-gray-500 dark:text-dark-400">
          {{ t('admin.smartRouting.overview.blockedHint') }}
        </p>
      </div>

      <div class="card overflow-hidden">
        <table class="w-full text-sm">
          <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800 dark:text-dark-400">
            <tr>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.account') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.platform') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.reason') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('admin.smartRouting.overview.recoversIn') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="row in blockedAccounts"
              :key="row.account.id"
              class="border-t border-gray-100 dark:border-dark-700/50"
            >
              <td class="px-4 py-2">
                <div class="font-medium text-gray-900 dark:text-white">{{ row.account.name }}</div>
                <div class="text-xs text-gray-400">#{{ row.account.id }}</div>
              </td>
              <td class="px-4 py-2 text-gray-600 dark:text-gray-300">{{ row.account.platform }}</td>
              <td class="px-4 py-2">
                <div class="flex flex-wrap gap-1">
                  <span
                    v-for="gate in row.state.gates"
                    :key="gate"
                    :class="['badge text-xs', gateBadgeClass(gate)]"
                  >
                    {{ gateLabel(gate) }}
                  </span>
                </div>
              </td>
              <td class="px-4 py-2">
                <span v-if="row.countdown" class="font-mono text-gray-700 dark:text-gray-300">
                  {{ row.countdown }}
                </span>
                <span v-else class="text-xs text-red-600 dark:text-red-400">
                  {{ t('admin.smartRouting.overview.needsManualAction') }}
                </span>
              </td>
            </tr>
            <tr v-if="blockedAccounts.length === 0">
              <td colspan="4" class="px-4 py-8 text-center text-gray-400">
                {{ t('admin.smartRouting.overview.blockedEmpty') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'
import type { SystemSettings } from '@/api/admin/settings'
import { formatCountdown } from '@/utils/format'
import {
  resolveSchedulingGate,
  GATE_REASON_KEY,
  GATE_BADGE_CLASS,
  ttftRatioBadgeClass,
  formatMs,
  TTFT_WARN_RATIO,
  type SchedulingGate
} from '@/utils/schedulingState'

const props = defineProps<{
  accounts: Account[]
  settings: SystemSettings | null
  /** 由父组件的定时器驱动，让倒计时随时间刷新而无需各面板各自计时。 */
  nowMs: number
}>()

const { t } = useI18n()

const settings = computed(() => props.settings)

// ---------- 健康分模式：关闭 / 影子 / 生效 ----------
// 三态而非布尔：影子模式下分数照常采集，但完全不参与选号，
// 只显示「已开启」会让运维误以为排序已经生效。
const healthMode = computed<'off' | 'shadow' | 'active'>(() => {
  if (!props.settings?.scheduling_health_scoring_enabled) return 'off'
  return props.settings.scheduling_health_shadow_mode ? 'shadow' : 'active'
})

const healthModeLabel = computed(() => {
  switch (healthMode.value) {
    case 'active':
      return t('admin.smartRouting.overview.healthScoringActive')
    case 'shadow':
      return t('admin.smartRouting.overview.healthScoringShadow')
    default:
      return t('admin.smartRouting.overview.healthScoringOff')
  }
})

const healthModeHint = computed(() => {
  switch (healthMode.value) {
    case 'active':
      return t('admin.smartRouting.overview.healthScoringActiveHint')
    case 'shadow':
      return t('admin.smartRouting.overview.healthScoringShadowHint')
    default:
      return t('admin.smartRouting.overview.healthScoringOffHint')
  }
})

const healthModeBadgeClass = computed(() => {
  switch (healthMode.value) {
    case 'active':
      return 'badge-success'
    case 'shadow':
      return 'badge-warning'
    default:
      return 'badge-gray'
  }
})

// ---------- 闸门统计 ----------
interface GateBreakdown {
  total: number
  available: number
  inactive: number
  manual: number
  rateLimited: number
  overloaded: number
  tempUnschedulable: number
}

const breakdown = computed<GateBreakdown>(() => {
  const acc: GateBreakdown = {
    total: 0,
    available: 0,
    inactive: 0,
    manual: 0,
    rateLimited: 0,
    overloaded: 0,
    tempUnschedulable: 0
  }
  for (const account of props.accounts) {
    acc.total++
    const state = resolveSchedulingGate(account, props.nowMs)
    if (state.gates.includes('rate_limited') || state.scopedRateLimits.length > 0) {
      acc.rateLimited++
    }
    if (state.available) {
      acc.available++
      continue
    }
    // 一个账号可能同时命中多个闸门，每类各计一次 ——
    // 这几个计数是「有多少账号受此闸门影响」，不是互斥分区，故不要求求和等于 total。
    if (state.gates.includes('inactive')) acc.inactive++
    if (state.gates.includes('manual')) acc.manual++
    if (state.gates.includes('overloaded')) acc.overloaded++
    if (state.gates.includes('temp_unschedulable')) acc.tempUnschedulable++
  }
  return acc
})

const gateStats = computed(() => [
  {
    key: 'total',
    label: t('admin.smartRouting.overview.totalAccounts'),
    value: breakdown.value.total,
    class: 'text-gray-900 dark:text-white'
  },
  {
    key: 'schedulable',
    label: t('admin.smartRouting.overview.schedulable'),
    value: breakdown.value.available,
    class: 'text-green-600 dark:text-green-400'
  },
  {
    key: 'inactive',
    label: t('admin.smartRouting.overview.inactive'),
    value: breakdown.value.inactive,
    class: breakdown.value.inactive > 0 ? 'text-red-600 dark:text-red-400' : 'text-gray-400'
  },
  {
    key: 'manual',
    label: t('admin.smartRouting.overview.manuallyDisabled'),
    value: breakdown.value.manual,
    class: breakdown.value.manual > 0 ? 'text-gray-600 dark:text-gray-300' : 'text-gray-400'
  },
  {
    key: 'rateLimited',
    label: t('admin.smartRouting.overview.rateLimited'),
    value: breakdown.value.rateLimited,
    class: breakdown.value.rateLimited > 0 ? 'text-amber-600 dark:text-amber-400' : 'text-gray-400'
  },
  {
    key: 'overloaded',
    label: t('admin.smartRouting.overview.overloaded'),
    value: breakdown.value.overloaded,
    class: breakdown.value.overloaded > 0 ? 'text-amber-600 dark:text-amber-400' : 'text-gray-400'
  },
  {
    key: 'blocked',
    label: t('admin.smartRouting.overview.tempUnschedulable'),
    value: breakdown.value.tempUnschedulable,
    class:
      breakdown.value.tempUnschedulable > 0 ? 'text-amber-600 dark:text-amber-400' : 'text-gray-400'
  }
])

// ---------- 平台 / 分组分布 ----------
interface DistributionRow {
  key: string
  total: number
  available: number
}

function buildDistribution(keyOf: (account: Account) => string[]): DistributionRow[] {
  const map = new Map<string, DistributionRow>()
  for (const account of props.accounts) {
    const available = resolveSchedulingGate(account, props.nowMs).available
    for (const key of keyOf(account)) {
      let row = map.get(key)
      if (!row) {
        row = { key, total: 0, available: 0 }
        map.set(key, row)
      }
      row.total++
      if (available) row.available++
    }
  }
  return [...map.values()].sort((a, b) => b.total - a.total || a.key.localeCompare(b.key))
}

const byPlatform = computed(() => buildDistribution((a) => [a.platform || t('admin.smartRouting.common.unknown')]))

const byGroup = computed(() =>
  // 账号可同属多个分组，这里按「分组视角」统计，因此各行之和会大于账号总数。
  buildDistribution((a) => {
    const names = (a.groups ?? []).map((g) => g.name).filter(Boolean)
    return names.length > 0 ? names : [t('admin.smartRouting.overview.ungrouped')]
  })
)

// ---------- 变慢但没报错的账号 ----------
// 这类账号硬闸门全绿、健康分（在 TTFT 扣分未开启时）也是满分，
// 传统的「可用性」视角完全看不到它们，但用户体感最差。

const ttftMonitored = computed(() => props.accounts.some((a) => a.ttft))
const ttftDegradeEnabled = computed(() => !!props.settings?.scheduling_ttft_degrade_enabled)

const slowAccounts = computed(() => {
  return props.accounts
    .filter((account) => account.ttft && account.ttft.ratio >= TTFT_WARN_RATIO)
    .map((account) => {
      const ttft = account.ttft!
      // 从明细里反推基线：样本最多的那个模型的基线最有代表性。
      const primary = (ttft.models ?? []).find((m) => m.baseline_ms > 0)
      return { account, ttft, baselineMs: primary?.baseline_ms ?? 0 }
    })
    .sort((a, b) => b.ttft.ratio - a.ttft.ratio)
})

// ---------- 不可调度账号 ----------
const blockedAccounts = computed(() => {
  return props.accounts
    .map((account) => ({
      account,
      state: resolveSchedulingGate(account, props.nowMs)
    }))
    .filter((row) => !row.state.available)
    .map((row) => ({
      ...row,
      countdown: row.state.recoversAt ? formatCountdown(row.state.recoversAt) : null
    }))
    .sort((a, b) => {
      // 有恢复时间的排在前面并按剩余时间升序；需要人工处理的沉底，
      // 因为它们不会自己好转，放在列表末尾反而更容易被当作待办清单看。
      const aAt = a.state.recoversAt ? new Date(a.state.recoversAt).getTime() : Infinity
      const bAt = b.state.recoversAt ? new Date(b.state.recoversAt).getTime() : Infinity
      if (aAt !== bAt) return aAt - bAt
      return a.account.id - b.account.id
    })
})

function gateLabel(gate: SchedulingGate): string {
  if (gate === 'available') return t('admin.smartRouting.accounts.gateAvailable')
  return t(`admin.smartRouting.overview.${GATE_REASON_KEY[gate]}`)
}

function gateBadgeClass(gate: SchedulingGate): string {
  return GATE_BADGE_CLASS[gate]
}
</script>
