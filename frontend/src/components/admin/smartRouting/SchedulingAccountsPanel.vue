<template>
  <div class="space-y-4">
    <!-- 过滤器 -->
    <div class="grid grid-cols-2 gap-2 sm:flex sm:flex-wrap sm:items-center">
      <SearchInput
        v-model="search"
        :placeholder="t('admin.smartRouting.accounts.searchPlaceholder')"
        class="col-span-2 w-full sm:w-64"
      />
      <Select v-model="platformFilter" :options="platformOptions" class="w-full sm:w-40" />
      <Select v-model="groupFilter" :options="groupOptions" class="w-full sm:w-44" />
      <Select v-model="gateFilter" :options="gateOptions" class="w-full sm:w-40" />

      <div class="col-span-2 flex sm:ml-auto">
        <RouterLink to="/admin/accounts" class="btn btn-secondary btn-sm w-full sm:w-auto">
          {{ t('admin.smartRouting.common.goToAccount') }}
        </RouterLink>
      </div>
    </div>

    <p class="text-xs text-gray-500 dark:text-dark-400">
      {{ t('admin.smartRouting.accounts.healthSoftSignalHint') }}
    </p>

    <div class="card overflow-hidden">
      <DataTable :columns="columns" :data="rows" :loading="loading">
        <template #cell-account="{ row }">
          <div class="min-w-0">
            <div class="truncate font-medium text-gray-900 dark:text-white">
              {{ (row as AccountRow).account.name }}
            </div>
            <div class="text-xs text-gray-400">#{{ (row as AccountRow).account.id }}</div>
          </div>
        </template>

        <template #cell-platform="{ row }">
          <span class="text-sm text-gray-600 dark:text-gray-300">
            {{ (row as AccountRow).account.platform }}
          </span>
        </template>

        <template #cell-groups="{ row }">
          <span class="text-sm text-gray-600 dark:text-gray-300">
            {{ groupsLabel((row as AccountRow).account) }}
          </span>
        </template>

        <template #cell-gate="{ row }">
          <div class="flex flex-wrap items-center gap-1">
            <span
              v-if="(row as AccountRow).state.available"
              class="badge badge-success text-xs"
            >
              {{ t('admin.smartRouting.accounts.gateAvailable') }}
            </span>
            <template v-else>
              <span
                v-for="gate in (row as AccountRow).state.gates"
                :key="gate"
                :class="['badge text-xs', gateBadgeClass(gate)]"
              >
                {{ gateLabel(gate) }}
              </span>
            </template>
            <span
              v-for="limit in (row as AccountRow).state.scopedRateLimits"
              :key="`${limit.scope}:${limit.recoversAt}`"
              class="badge badge-warning text-xs"
              :title="limit.recoversAt"
            >
              {{
                t('admin.smartRouting.accounts.modelRateLimited', {
                  model: limit.scope,
                  countdown: formatCountdown(limit.recoversAt)
                })
              }}
            </span>
          </div>
          <div
            v-if="!(row as AccountRow).state.available && (row as AccountRow).countdown"
            class="mt-0.5 font-mono text-xs text-gray-500 dark:text-dark-400"
          >
            {{ (row as AccountRow).countdown }}
          </div>
        </template>

        <template #cell-health="{ row }">
          <div v-if="(row as AccountRow).account.health_score != null" class="flex items-center gap-1.5">
            <span class="font-mono text-sm text-gray-700 dark:text-gray-300">
              {{ Math.round((row as AccountRow).account.health_score as number) }}
            </span>
            <span
              :class="['badge text-xs', healthTierBadgeClass((row as AccountRow).account.health_tier)]"
              :title="healthTierHint((row as AccountRow).account.health_tier)"
            >
              {{ healthTierLabel((row as AccountRow).account.health_tier) }}
            </span>
          </div>
          <span v-else class="text-xs text-gray-400">
            {{ t('admin.smartRouting.accounts.healthDisabled') }}
          </span>
        </template>

        <template #cell-ttft="{ row }">
          <div v-if="(row as AccountRow).account.ttft" class="space-y-0.5">
            <div class="flex items-center gap-1.5">
              <span class="font-mono text-sm text-gray-700 dark:text-gray-300">
                {{ formatMs((row as AccountRow).account.ttft!.p50_ms) }}
              </span>
              <span
                v-if="(row as AccountRow).account.ttft!.ratio > 0"
                :class="['badge text-xs', ttftRatioBadgeClass((row as AccountRow).account.ttft!)]"
                :title="ttftTooltip((row as AccountRow).account.ttft!)"
              >
                {{ (row as AccountRow).account.ttft!.ratio.toFixed(1) }}×
              </span>
            </div>
            <div class="text-xs text-gray-400">
              P95 {{ formatMs((row as AccountRow).account.ttft!.p95_ms) }} ·
              {{ (row as AccountRow).account.ttft!.samples }}
            </div>
          </div>
          <span v-else class="text-xs text-gray-400">—</span>
        </template>

        <template #cell-priority="{ row }">
          <span class="font-mono text-sm text-gray-600 dark:text-gray-300">
            {{ (row as AccountRow).account.priority }}
          </span>
        </template>

        <template #cell-load="{ row }">
          <span class="font-mono text-sm text-gray-600 dark:text-gray-300">
            {{ (row as AccountRow).account.current_concurrency ?? 0 }} / {{ (row as AccountRow).account.concurrency || '∞' }}
          </span>
        </template>

        <template #cell-runtime="{ row }">
          <div class="space-y-0.5 text-xs text-gray-500 dark:text-dark-400">
            <div v-if="(row as AccountRow).account.current_rpm != null">
              {{ t('admin.smartRouting.accounts.rpm') }}: {{ (row as AccountRow).account.current_rpm }}
            </div>
            <div v-if="(row as AccountRow).account.current_window_cost != null">
              {{ t('admin.smartRouting.accounts.windowCost') }}:
              ${{ ((row as AccountRow).account.current_window_cost as number).toFixed(2) }}
            </div>
            <div v-if="(row as AccountRow).account.active_sessions != null">
              {{ t('admin.smartRouting.accounts.sessions') }}: {{ (row as AccountRow).account.active_sessions }}
            </div>
            <div
              v-if="
                (row as AccountRow).account.current_rpm == null &&
                (row as AccountRow).account.current_window_cost == null &&
                (row as AccountRow).account.active_sessions == null
              "
              class="text-gray-400"
            >
              —
            </div>
          </div>
        </template>

        <template #cell-lastUsed="{ row }">
          <span class="text-xs text-gray-500 dark:text-dark-400">
            {{
              (row as AccountRow).account.last_used_at
                ? formatRelativeTime((row as AccountRow).account.last_used_at)
                : t('admin.smartRouting.common.never')
            }}
          </span>
        </template>

        <template #empty>
          <EmptyState :title="t('admin.smartRouting.accounts.empty')" />
        </template>
      </DataTable>
    </div>

    <Pagination
      v-if="total > pageSize"
      :page="page"
      :page-size="pageSize"
      :total="total"
      @update:page="onPageChange"
      @update:page-size="onPageSizeChange"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink } from 'vue-router'
import { adminAPI } from '@/api/admin'
import type { Account, AccountTTFTSnapshot, AdminGroup } from '@/types'
import type { Column } from '@/components/common/types'
import { formatCountdown, formatRelativeTime } from '@/utils/format'
import {
  resolveSchedulingGate,
  GATE_REASON_KEY,
  GATE_BADGE_CLASS,
  healthTierKey,
  healthTierBadgeClass,
  ttftRatioBadgeClass,
  formatMs,
  type SchedulingGate,
  type SchedulingGateState
} from '@/utils/schedulingState'

import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import SearchInput from '@/components/common/SearchInput.vue'
import EmptyState from '@/components/common/EmptyState.vue'

const props = defineProps<{
  groups: AdminGroup[]
  /** 由父组件的定时器驱动，保证倒计时与总览页同源刷新。 */
  nowMs: number
}>()

const { t } = useI18n()

interface AccountRow {
  id: number
  account: Account
  state: SchedulingGateState
  countdown: string | null
}

const accounts = ref<Account[]>([])
const loading = ref(false)
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

const search = ref('')
const platformFilter = ref('')
const groupFilter = ref('')
// 闸门过滤在前端做：闸门状态由多个字段（429/529/temp_unsched/schedulable/status）
// 组合推导，后端账号列表接口没有对应的查询参数。
const gateFilter = ref<'' | 'available' | 'blocked'>('')

const platformOptions = computed(() => [
  { value: '', label: t('admin.smartRouting.accounts.filterPlatform') + ': ' + t('admin.smartRouting.common.all') },
  { value: 'anthropic', label: 'anthropic' },
  { value: 'openai', label: 'openai' },
  { value: 'gemini', label: 'gemini' },
  { value: 'antigravity', label: 'antigravity' },
  { value: 'grok', label: 'grok' }
])

const groupOptions = computed(() => [
  { value: '', label: t('admin.smartRouting.accounts.filterGroup') + ': ' + t('admin.smartRouting.common.all') },
  ...props.groups.map((g) => ({ value: String(g.id), label: g.name }))
])

const gateOptions = computed(() => [
  { value: '', label: t('admin.smartRouting.accounts.statusAll') },
  { value: 'available', label: t('admin.smartRouting.accounts.statusAvailable') },
  { value: 'blocked', label: t('admin.smartRouting.accounts.statusBlocked') }
])

const columns = computed<Column[]>(() => [
  { key: 'account', label: t('admin.smartRouting.accounts.colAccount') },
  { key: 'platform', label: t('admin.smartRouting.accounts.colPlatform') },
  { key: 'groups', label: t('admin.smartRouting.accounts.colGroups') },
  { key: 'gate', label: t('admin.smartRouting.accounts.colGate') },
  { key: 'health', label: t('admin.smartRouting.accounts.colHealth') },
  { key: 'ttft', label: t('admin.smartRouting.accounts.colTtft') },
  { key: 'priority', label: t('admin.smartRouting.accounts.colPriority') },
  { key: 'load', label: t('admin.smartRouting.accounts.colLoad') },
  { key: 'runtime', label: t('admin.smartRouting.accounts.colRuntime') },
  { key: 'lastUsed', label: t('admin.smartRouting.accounts.colLastUsed') }
])

const rows = computed<AccountRow[]>(() => {
  return accounts.value
    .map((account) => {
      const state = resolveSchedulingGate(account, props.nowMs)
      return {
        id: account.id,
        account,
        state,
        countdown: state.recoversAt ? formatCountdown(state.recoversAt) : null
      }
    })
    .filter((row) => {
      if (gateFilter.value === 'available') return row.state.available
      if (gateFilter.value === 'blocked') return !row.state.available
      return true
    })
})

function groupsLabel(account: Account): string {
  const names = (account.groups ?? []).map((g) => g.name).filter(Boolean)
  return names.length > 0 ? names.join(', ') : t('admin.smartRouting.overview.ungrouped')
}

function gateLabel(gate: SchedulingGate): string {
  if (gate === 'available') return t('admin.smartRouting.accounts.gateAvailable')
  return t(`admin.smartRouting.overview.${GATE_REASON_KEY[gate]}`)
}

function gateBadgeClass(gate: SchedulingGate): string {
  return GATE_BADGE_CLASS[gate]
}

function healthTierLabel(tier: number | null | undefined): string {
  return t(`admin.smartRouting.accounts.${healthTierKey(tier)}`)
}

function healthTierHint(tier: number | null | undefined): string {
  return t(`admin.smartRouting.accounts.${healthTierKey(tier)}Hint`)
}

/** 倍率徽章的悬浮说明：给出基线、最慢模型，让「为什么是这个倍率」可自证。 */
function ttftTooltip(snapshot: AccountTTFTSnapshot): string {
  const parts = [
    t('admin.smartRouting.accounts.ttftRatioTip', { ratio: snapshot.ratio.toFixed(2) })
  ]
  if (snapshot.worst_model && snapshot.worst_ratio > 0) {
    parts.push(
      t('admin.smartRouting.accounts.ttftWorstTip', {
        group: snapshot.worst_group_id ?? 0,
        model: snapshot.worst_model,
        ratio: snapshot.worst_ratio.toFixed(2)
      })
    )
  }
  for (const model of snapshot.models ?? []) {
    if (model.baseline_ms > 0) {
      parts.push(
        `#${model.group_id} · ${model.model}: ${formatMs(model.p50_ms)} / ${formatMs(model.baseline_ms)} = ${model.ratio.toFixed(2)}×`
      )
    } else {
      parts.push(
        `#${model.group_id} · ${model.model}: ${formatMs(model.p50_ms)} (${t('admin.smartRouting.accounts.ttftNoBaseline')})`
      )
    }
  }
  return parts.join('\n')
}

async function loadAccounts() {
  loading.value = true
  try {
    const res = await adminAPI.accounts.list(page.value, pageSize.value, {
      platform: platformFilter.value || undefined,
      group: groupFilter.value || undefined,
      search: search.value || undefined
    })
    accounts.value = res.items ?? []
    total.value = res.total ?? 0
  } finally {
    loading.value = false
  }
}

function onPageChange(next: number) {
  page.value = next
  loadAccounts()
}

function onPageSizeChange(next: number) {
  pageSize.value = next
  page.value = 1
  loadAccounts()
}

// SearchInput 自带 300ms 防抖，这里不再二次防抖，否则输入到刷新会有 600ms 延迟。
watch([search, platformFilter, groupFilter], () => {
  page.value = 1
  loadAccounts()
})

onMounted(loadAccounts)

defineExpose({ reload: loadAccounts })
</script>
