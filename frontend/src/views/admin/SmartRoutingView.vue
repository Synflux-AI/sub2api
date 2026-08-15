<template>
  <AppLayout>
    <div class="space-y-4">
      <div class="card">
        <!-- Tab 栏 -->
        <div class="flex flex-wrap items-center border-b border-gray-200 px-2 dark:border-dark-700 sm:px-4">
          <button
            v-for="tab in tabs"
            :key="tab.key"
            type="button"
            data-testid="smart-routing-tab"
            class="-mb-px inline-flex items-center gap-1.5 border-b-2 px-3 py-3 text-sm font-medium transition-colors sm:px-4"
            :class="
              activeTab === tab.key
                ? 'border-primary-500 text-primary-600 dark:text-primary-400'
                : 'border-transparent text-gray-500 hover:border-gray-300 hover:text-gray-700 dark:text-gray-400 dark:hover:border-dark-500 dark:hover:text-gray-200'
            "
            @click="switchTab(tab.key)"
          >
            <Icon :name="tab.icon" size="sm" />
            {{ tab.label }}
          </button>

          <div class="ml-auto flex items-center gap-2 py-2 pr-1">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              :disabled="sharedLoading"
              :title="t('admin.smartRouting.common.refresh')"
              @click="refreshAll"
            >
              <Icon name="refresh" size="sm" :class="sharedLoading ? 'animate-spin' : ''" />
            </button>
          </div>
        </div>

        <div class="p-4 sm:p-5">
          <p class="mb-4 text-sm text-gray-500 dark:text-dark-400">
            {{ t('admin.smartRouting.description') }}
          </p>

          <!-- 总览 -->
          <SchedulingOverviewPanel
            v-show="activeTab === 'overview'"
            :accounts="allAccounts"
            :settings="settings"
            :now-ms="nowMs"
          />

          <!-- 渠道账号：懒挂载，避免未访问的 Tab 也发起账号列表请求 -->
          <SchedulingAccountsPanel
            v-if="mounted.accounts"
            v-show="activeTab === 'accounts'"
            ref="accountsPanelRef"
            :groups="groups"
            :now-ms="nowMs"
          />

          <RoutingStrategiesPanel v-if="mounted.strategies" v-show="activeTab === 'strategies'" />

          <SchedulingSettingsPanel
            v-if="mounted.settings"
            v-show="activeTab === 'settings'"
            ref="settingsPanelRef"
            @saved="onSettingsSaved"
          />
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { adminAPI } from '@/api/admin'
import type { Account, AdminGroup } from '@/types'
import type { SystemSettings } from '@/api/admin/settings'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'

import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import SchedulingOverviewPanel from '@/components/admin/smartRouting/SchedulingOverviewPanel.vue'
import SchedulingAccountsPanel from '@/components/admin/smartRouting/SchedulingAccountsPanel.vue'
import SchedulingSettingsPanel from '@/components/admin/smartRouting/SchedulingSettingsPanel.vue'
import RoutingStrategiesPanel from '@/components/admin/smartRouting/RoutingStrategiesPanel.vue'

type TabKey = 'overview' | 'accounts' | 'strategies' | 'settings'

const TAB_KEYS: TabKey[] = ['overview', 'accounts', 'strategies', 'settings']

// 单页最多取 1000 条（后端 ParsePagination 的上限）。总览需要全量账号做统计，
// 因此分页拉取；上限 10 页兜底，防止异常数据把前端拖死。
const ACCOUNTS_PAGE_SIZE = 1000
const ACCOUNTS_MAX_PAGES = 10

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const appStore = useAppStore()

const activeTab = ref<TabKey>('overview')
// 除总览外的 Tab 懒挂载，首次切换时才创建组件并发起请求。
const mounted = reactive<Record<TabKey, boolean>>({
  overview: true,
  accounts: false,
  strategies: false,
  settings: false
})

const allAccounts = ref<Account[]>([])
const groups = ref<AdminGroup[]>([])
const settings = ref<SystemSettings | null>(null)
const sharedLoading = ref(false)

const accountsPanelRef = ref<InstanceType<typeof SchedulingAccountsPanel> | null>(null)
const settingsPanelRef = ref<InstanceType<typeof SchedulingSettingsPanel> | null>(null)

// 倒计时的统一时钟：各面板都用它算剩余时间，避免每个组件各起一个定时器，
// 也避免同一账号的倒计时在不同 Tab 上差几秒。
const nowMs = ref(Date.now())
let clockTimer: ReturnType<typeof setInterval> | null = null

const tabs = computed(() => [
  { key: 'overview' as const, label: t('admin.smartRouting.tabs.overview'), icon: 'chart' as const },
  { key: 'accounts' as const, label: t('admin.smartRouting.tabs.accounts'), icon: 'globe' as const },
  { key: 'strategies' as const, label: t('admin.smartRouting.tabs.strategies'), icon: 'swap' as const },
  { key: 'settings' as const, label: t('admin.smartRouting.tabs.settings'), icon: 'cog' as const }
])

function switchTab(key: TabKey) {
  activeTab.value = key
  mounted[key] = true
  // 写进 query 而非 path，这样刷新/分享链接能回到同一个 Tab，
  // 同时旧路由 /admin/routing-strategies 可以重定向到 ?tab=strategies。
  if (route.query.tab !== key) {
    router.replace({ query: { ...route.query, tab: key } })
  }
}

function resolveTabFromRoute(): TabKey {
  const raw = route.query.tab
  const value = Array.isArray(raw) ? raw[0] : raw
  return TAB_KEYS.includes(value as TabKey) ? (value as TabKey) : 'overview'
}

async function loadAllAccounts() {
  const collected: Account[] = []
  for (let page = 1; page <= ACCOUNTS_MAX_PAGES; page++) {
    const res = await adminAPI.accounts.list(page, ACCOUNTS_PAGE_SIZE)
    const items = res.items ?? []
    collected.push(...items)
    if (collected.length >= (res.total ?? 0) || items.length < ACCOUNTS_PAGE_SIZE) break
  }
  allAccounts.value = collected
}

async function loadShared() {
  sharedLoading.value = true
  try {
    const [, groupList, systemSettings] = await Promise.all([
      loadAllAccounts(),
      adminAPI.groups.getAll(),
      adminAPI.settings.getSettings()
    ])
    groups.value = groupList ?? []
    settings.value = systemSettings
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error))
  } finally {
    sharedLoading.value = false
  }
}

function refreshAll() {
  nowMs.value = Date.now()
  loadShared()
  accountsPanelRef.value?.reload()
  settingsPanelRef.value?.reload()
}

function onSettingsSaved(updated: SystemSettings) {
  // 设置页保存后同步总览的状态灯，省掉一次手动刷新。
  settings.value = updated
}

watch(
  () => route.query.tab,
  () => {
    const next = resolveTabFromRoute()
    activeTab.value = next
    mounted[next] = true
  }
)

onMounted(() => {
  const initial = resolveTabFromRoute()
  activeTab.value = initial
  mounted[initial] = true
  loadShared()
  clockTimer = setInterval(() => {
    nowMs.value = Date.now()
  }, 30_000)
})

onUnmounted(() => {
  if (clockTimer) clearInterval(clockTimer)
})
</script>
