<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <div class="min-w-0 flex-1">
            <h2 class="text-sm text-gray-500 dark:text-dark-400">
              {{ t('admin.routingStrategies.description') }}
            </h2>
          </div>
          <div class="flex flex-wrap items-center justify-end gap-2">
            <button @click="loadStrategies" :disabled="loading" class="btn btn-secondary" :title="t('common.refresh')">
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button @click="openTester" class="btn btn-secondary">
              {{ t('admin.routingStrategies.tester') }}
            </button>
            <button @click="openCreateDialog" class="btn btn-primary">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t('admin.routingStrategies.create') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable :columns="columns" :data="strategies" :loading="loading">
          <template #cell-name="{ row }">
            <div class="min-w-0">
              <div class="truncate font-medium text-gray-900 dark:text-white">{{ row.name }}</div>
              <div v-if="row.description" class="mt-0.5 truncate text-xs text-gray-500 dark:text-dark-400">
                {{ row.description }}
              </div>
              <div class="mt-0.5 text-xs text-gray-400">#{{ row.id }}</div>
            </div>
          </template>

          <template #cell-scope="{ row }">
            <div class="text-sm text-gray-600 dark:text-gray-300">
              <div>{{ row.platform ? row.platform : t('admin.routingStrategies.anyPlatform') }}</div>
              <div class="text-xs text-gray-400">{{ groupLabel(row.group_id) }}</div>
            </div>
          </template>

          <template #cell-conditions="{ row }">
            <span class="text-sm text-gray-600 dark:text-gray-300">{{ conditionsSummary(row) }}</span>
          </template>

          <template #cell-action="{ row }">
            <span :class="['badge', row.action === 'restrict' ? 'badge-warning' : 'badge-gray']">
              {{ row.action === 'restrict' ? t('admin.routingStrategies.actionRestrict') : t('admin.routingStrategies.actionPrefer') }}
            </span>
          </template>

          <template #cell-accounts="{ row }">
            <span class="text-sm text-gray-600 dark:text-gray-300">{{ accountsSummary(row.account_ids) }}</span>
          </template>

          <template #cell-enabled="{ row }">
            <span :class="['badge', row.enabled ? 'badge-success' : 'badge-gray']">
              {{ row.enabled ? t('admin.routingStrategies.enabled') : t('admin.routingStrategies.disabled') }}
            </span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center space-x-1">
              <button
                @click="openEditDialog(row)"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-300"
                :title="t('common.edit')"
              >
                <Icon name="edit" size="sm" />
              </button>
              <button
                @click="handleDelete(row)"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
                :title="t('common.delete')"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('admin.routingStrategies.empty')"
              :action-text="t('admin.routingStrategies.create')"
              @action="openCreateDialog"
            />
          </template>
        </DataTable>
      </template>
    </TablePageLayout>

    <!-- Create/Edit Dialog -->
    <BaseDialog
      :show="showEditDialog"
      :title="isEditing ? t('admin.routingStrategies.edit') : t('admin.routingStrategies.create')"
      width="wide"
      @close="closeEdit"
    >
      <form id="routing-strategy-form" @submit.prevent="handleSave" class="space-y-4">
        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.name') }}</label>
            <input v-model="form.name" type="text" class="input" required />
          </div>
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.priority') }}</label>
            <input v-model.number="form.priority" type="number" min="0" class="input" />
            <p class="input-hint">{{ t('admin.routingStrategies.priorityHint') }}</p>
          </div>
        </div>

        <div>
          <label class="input-label">{{ t('admin.routingStrategies.strategyDescription') }}</label>
          <input v-model="form.description" type="text" class="input" />
        </div>

        <div class="grid grid-cols-1 gap-4 md:grid-cols-3">
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.platform') }}</label>
            <Select v-model="form.platform" :options="platformOptions" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.group') }}</label>
            <Select v-model="form.group_id" :options="groupOptions" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.action') }}</label>
            <Select v-model="form.action" :options="actionOptions" />
          </div>
        </div>

        <!-- Conditions -->
        <div>
          <div class="mb-2 flex items-center justify-between">
            <label class="input-label mb-0">{{ t('admin.routingStrategies.conditions') }}</label>
            <div class="flex items-center gap-2">
              <Select v-model="form.match_mode" :options="matchModeOptions" class="w-44" />
              <button type="button" @click="addCondition" class="btn btn-secondary btn-sm">
                <Icon name="plus" size="sm" class="mr-1" />
                {{ t('admin.routingStrategies.addCondition') }}
              </button>
            </div>
          </div>

          <p v-if="form.conditions.length === 0" class="input-hint">
            {{ t('admin.routingStrategies.noConditions') }}
          </p>

          <div
            v-for="(cond, idx) in form.conditions"
            :key="idx"
            class="mb-2 grid grid-cols-12 items-center gap-2"
          >
            <Select
              :model-value="cond.type"
              :options="conditionTypeOptions"
              class="col-span-3"
              @update:model-value="(v) => onConditionTypeChange(idx, v as string)"
            />
            <Select
              v-if="cond.type !== 'client'"
              v-model="cond.op"
              :options="opOptionsFor(cond.type)"
              class="col-span-3"
            />
            <Select
              v-if="cond.type === 'client'"
              v-model="cond.value"
              :options="clientOptions"
              class="col-span-8"
            />
            <input
              v-else
              v-model="cond.value"
              type="text"
              class="input col-span-5"
              :placeholder="conditionValuePlaceholder(cond.type)"
            />
            <button
              type="button"
              @click="removeCondition(idx)"
              class="col-span-1 flex justify-center rounded-lg p-1.5 text-gray-500 hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
              :title="t('common.delete')"
            >
              <Icon name="trash" size="sm" />
            </button>
          </div>
        </div>

        <!-- Target accounts -->
        <div>
          <label class="input-label">{{ t('admin.routingStrategies.accounts') }}</label>
          <p class="input-hint">{{ t('admin.routingStrategies.accountsHint') }}</p>
          <input
            v-model="accountSearch"
            type="text"
            class="input mt-1"
            :placeholder="t('admin.routingStrategies.selectAccounts')"
          />
          <div class="mt-2 max-h-48 overflow-y-auto rounded-lg border border-gray-200 p-2 dark:border-dark-600">
            <label
              v-for="acc in filteredAccounts"
              :key="acc.id"
              class="flex cursor-pointer items-center gap-2 rounded px-2 py-1 hover:bg-gray-50 dark:hover:bg-dark-700"
            >
              <input
                type="checkbox"
                :checked="form.account_ids.includes(acc.id)"
                @change="toggleAccount(acc.id)"
              />
              <span class="text-sm text-gray-800 dark:text-gray-200">{{ acc.name }}</span>
              <span class="text-xs text-gray-400">{{ acc.platform }} · #{{ acc.id }}</span>
            </label>
            <p v-if="filteredAccounts.length === 0" class="px-2 py-1 text-sm text-gray-400">
              {{ t('empty.noData') }}
            </p>
          </div>
        </div>

        <div class="flex items-center gap-2">
          <Toggle v-model="form.enabled" />
          <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.routingStrategies.enabled') }}</span>
        </div>
      </form>

      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" @click="closeEdit" class="btn btn-secondary">{{ t('common.cancel') }}</button>
          <button type="submit" form="routing-strategy-form" :disabled="saving" class="btn btn-primary">
            {{ saving ? t('common.saving') : t('common.save') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- Tester Dialog -->
    <BaseDialog :show="showTester" :title="t('admin.routingStrategies.testerTitle')" @close="showTester = false">
      <form id="routing-tester-form" @submit.prevent="runTest" class="space-y-4">
        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.platform') }}</label>
            <Select v-model="testForm.platform" :options="platformOptions" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.group') }}</label>
            <Select v-model="testForm.group_id" :options="groupOptions" />
          </div>
        </div>
        <div>
          <label class="input-label">{{ t('admin.routingStrategies.testModel') }}</label>
          <input v-model="testForm.model" type="text" class="input" placeholder="claude-opus-4-20250514" />
        </div>
        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.testClient') }}</label>
            <Select v-model="testForm.client" :options="clientOptions" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.routingStrategies.testUserAgent') }}</label>
            <input v-model="testForm.user_agent" type="text" class="input" placeholder="claude-cli/2.1.0" />
          </div>
        </div>

        <div v-if="testResult" class="rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600">
          <p v-if="!testResult.matched" class="text-gray-500">{{ t('admin.routingStrategies.testNoMatch') }}</p>
          <template v-else>
            <p class="font-medium text-gray-900 dark:text-white">
              {{ t('admin.routingStrategies.testMatched', { name: testResult.strategy_name, id: testResult.strategy_id }) }}
            </p>
            <p class="mt-1 text-gray-600 dark:text-gray-300">
              {{ t('admin.routingStrategies.testResultAction') }}:
              {{ testResult.action === 'restrict' ? t('admin.routingStrategies.actionRestrict') : t('admin.routingStrategies.actionPrefer') }}
            </p>
            <p class="mt-1 text-gray-600 dark:text-gray-300">
              {{ t('admin.routingStrategies.testResultAccounts') }}: {{ accountsSummary(testResult.account_ids) }}
            </p>
          </template>
        </div>
      </form>

      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" @click="showTester = false" class="btn btn-secondary">{{ t('common.cancel') }}</button>
          <button type="submit" form="routing-tester-form" :disabled="testing" class="btn btn-primary">
            {{ t('admin.routingStrategies.runTest') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- Delete Confirmation -->
    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('common.delete')"
      :message="deleteMessage"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type {
  Account,
  AdminGroup,
  RoutingStrategy,
  RoutingCondition,
  RoutingConditionType,
  SaveRoutingStrategyRequest,
  TestRoutingStrategyResult
} from '@/types'
import type { Column } from '@/components/common/types'

import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()
const appStore = useAppStore()

const strategies = ref<RoutingStrategy[]>([])
const loading = ref(false)
const groups = ref<AdminGroup[]>([])
const accounts = ref<Account[]>([])

const columns = computed<Column[]>(() => [
  { key: 'name', label: t('admin.routingStrategies.name') },
  { key: 'scope', label: t('admin.routingStrategies.platform') },
  { key: 'conditions', label: t('admin.routingStrategies.conditions') },
  { key: 'action', label: t('admin.routingStrategies.action') },
  { key: 'accounts', label: t('admin.routingStrategies.accounts') },
  { key: 'priority', label: t('admin.routingStrategies.priority') },
  { key: 'enabled', label: t('admin.routingStrategies.enabled') },
  { key: 'actions', label: t('common.actions') }
])

const platformOptions = computed(() => [
  { value: '', label: t('admin.routingStrategies.anyPlatform') },
  { value: 'anthropic', label: 'anthropic' },
  { value: 'openai', label: 'openai' },
  { value: 'gemini', label: 'gemini' },
  { value: 'antigravity', label: 'antigravity' }
])

const groupOptions = computed(() => [
  { value: null as number | null, label: t('admin.routingStrategies.globalScope') },
  ...groups.value.map((g) => ({ value: g.id, label: g.name }))
])

const actionOptions = computed(() => [
  { value: 'restrict', label: t('admin.routingStrategies.actionRestrict') },
  { value: 'prefer', label: t('admin.routingStrategies.actionPrefer') }
])

const matchModeOptions = computed(() => [
  { value: 'all', label: t('admin.routingStrategies.matchAll') },
  { value: 'any', label: t('admin.routingStrategies.matchAny') }
])

const conditionTypeOptions = computed(() => [
  { value: 'model', label: t('admin.routingStrategies.typeModel') },
  { value: 'client', label: t('admin.routingStrategies.typeClient') },
  { value: 'user_agent', label: t('admin.routingStrategies.typeUserAgent') }
])

const clientOptions = computed(() => [
  { value: 'claude_code', label: t('admin.routingStrategies.clientClaudeCode') },
  { value: 'codex', label: t('admin.routingStrategies.clientCodex') },
  { value: 'other', label: t('admin.routingStrategies.clientOther') },
  { value: 'any', label: t('admin.routingStrategies.clientAny') }
])

function opOptionsFor(type: RoutingConditionType) {
  if (type === 'user_agent') {
    return [
      { value: 'contains', label: t('admin.routingStrategies.opContains') },
      { value: 'regex', label: t('admin.routingStrategies.opRegex') }
    ]
  }
  // model
  return [
    { value: 'wildcard', label: t('admin.routingStrategies.opWildcard') },
    { value: 'exact', label: t('admin.routingStrategies.opExact') },
    { value: 'regex', label: t('admin.routingStrategies.opRegex') }
  ]
}

function conditionValuePlaceholder(type: RoutingConditionType) {
  if (type === 'model') return 'claude-opus-*'
  if (type === 'user_agent') return 'claude-cli'
  return ''
}

function groupLabel(groupId: number | null) {
  if (groupId == null) return t('admin.routingStrategies.globalScope')
  const g = groups.value.find((x) => x.id === groupId)
  return g ? g.name : `#${groupId}`
}

function conditionsSummary(row: RoutingStrategy) {
  if (!row.conditions || row.conditions.length === 0) return t('admin.routingStrategies.noConditions')
  return row.conditions
    .map((c) => {
      if (c.type === 'client') return `client=${c.value}`
      return `${c.type}:${c.op || ''} ${c.value}`.trim()
    })
    .join(row.match_mode === 'any' ? ' | ' : ' & ')
}

function accountsSummary(ids: number[]) {
  if (!ids || ids.length === 0) return '-'
  const names = ids.map((id) => {
    const a = accounts.value.find((x) => x.id === id)
    return a ? a.name : `#${id}`
  })
  return names.join(', ')
}

const accountSearch = ref('')
const filteredAccounts = computed(() => {
  const q = accountSearch.value.trim().toLowerCase()
  return accounts.value.filter((a) => {
    if (form.platform && a.platform !== form.platform) return false
    if (q && !a.name.toLowerCase().includes(q) && !String(a.id).includes(q)) return false
    return true
  })
})

async function loadStrategies() {
  try {
    loading.value = true
    strategies.value = await adminAPI.routingStrategies.list()
  } catch (error: any) {
    console.error('Error loading routing strategies:', error)
    appStore.showError(error.response?.data?.detail || t('admin.routingStrategies.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function loadGroups() {
  try {
    groups.value = (await adminAPI.groups.getAll()) || []
  } catch (error) {
    console.error('Error loading groups:', error)
  }
}

async function loadAccounts() {
  try {
    const res = await adminAPI.accounts.list(1, 500)
    accounts.value = res.items || []
  } catch (error) {
    console.error('Error loading accounts:', error)
  }
}

// ===== Create/Edit =====
const showEditDialog = ref(false)
const saving = ref(false)
const editing = ref<RoutingStrategy | null>(null)
const isEditing = computed(() => !!editing.value)

const form = reactive<{
  name: string
  description: string
  enabled: boolean
  priority: number
  platform: string
  group_id: number | null
  match_mode: 'all' | 'any'
  conditions: RoutingCondition[]
  action: 'restrict' | 'prefer'
  account_ids: number[]
}>({
  name: '',
  description: '',
  enabled: true,
  priority: 100,
  platform: 'anthropic',
  group_id: null,
  match_mode: 'all',
  conditions: [],
  action: 'restrict',
  account_ids: []
})

function resetForm() {
  form.name = ''
  form.description = ''
  form.enabled = true
  form.priority = 100
  form.platform = 'anthropic'
  form.group_id = null
  form.match_mode = 'all'
  form.conditions = []
  form.action = 'restrict'
  form.account_ids = []
  accountSearch.value = ''
}

function openCreateDialog() {
  editing.value = null
  resetForm()
  showEditDialog.value = true
}

function openEditDialog(row: RoutingStrategy) {
  editing.value = row
  form.name = row.name
  form.description = row.description || ''
  form.enabled = row.enabled
  form.priority = row.priority
  form.platform = row.platform || ''
  form.group_id = row.group_id
  form.match_mode = row.match_mode || 'all'
  form.conditions = (row.conditions || []).map((c) => ({ ...c }))
  form.action = row.action || 'restrict'
  form.account_ids = [...(row.account_ids || [])]
  accountSearch.value = ''
  showEditDialog.value = true
}

function closeEdit() {
  showEditDialog.value = false
  editing.value = null
}

function addCondition() {
  form.conditions.push({ type: 'model', op: 'wildcard', value: '' })
}

function removeCondition(idx: number) {
  form.conditions.splice(idx, 1)
}

function onConditionTypeChange(idx: number, type: string) {
  const c = form.conditions[idx]
  c.type = type as RoutingConditionType
  if (type === 'client') {
    c.op = ''
    c.value = 'claude_code'
  } else if (type === 'user_agent') {
    c.op = 'contains'
    c.value = ''
  } else {
    c.op = 'wildcard'
    c.value = ''
  }
}

function toggleAccount(id: number) {
  const i = form.account_ids.indexOf(id)
  if (i === -1) form.account_ids.push(id)
  else form.account_ids.splice(i, 1)
}

function buildPayload(): SaveRoutingStrategyRequest {
  return {
    name: form.name.trim(),
    description: form.description.trim(),
    enabled: form.enabled,
    priority: form.priority,
    platform: form.platform,
    group_id: form.group_id,
    match_mode: form.match_mode,
    conditions: form.conditions.map((c) => ({
      type: c.type,
      op: c.type === 'client' ? '' : c.op,
      value: c.value.trim()
    })),
    action: form.action,
    account_ids: form.account_ids
  }
}

async function handleSave() {
  if (!form.name.trim()) {
    appStore.showError(t('admin.routingStrategies.nameRequired'))
    return
  }
  if (form.account_ids.length === 0) {
    appStore.showError(t('admin.routingStrategies.accountsRequired'))
    return
  }
  for (const c of form.conditions) {
    if (c.type !== 'client' && !c.value.trim()) {
      appStore.showError(t('admin.routingStrategies.valueRequired'))
      return
    }
  }

  saving.value = true
  try {
    const payload = buildPayload()
    if (editing.value) {
      await adminAPI.routingStrategies.update(editing.value.id, payload)
      appStore.showSuccess(t('admin.routingStrategies.updated'))
    } else {
      await adminAPI.routingStrategies.create(payload)
      appStore.showSuccess(t('admin.routingStrategies.created'))
    }
    showEditDialog.value = false
    editing.value = null
    await loadStrategies()
  } catch (error: any) {
    console.error('Failed to save routing strategy:', error)
    appStore.showError(error.response?.data?.detail || t('admin.routingStrategies.saveFailed'))
  } finally {
    saving.value = false
  }
}

// ===== Delete =====
const showDeleteDialog = ref(false)
const deleting = ref<RoutingStrategy | null>(null)
const deleteMessage = computed(() =>
  deleting.value ? t('admin.routingStrategies.deleteConfirm', { name: deleting.value.name }) : ''
)

function handleDelete(row: RoutingStrategy) {
  deleting.value = row
  showDeleteDialog.value = true
}

async function confirmDelete() {
  if (!deleting.value) return
  try {
    await adminAPI.routingStrategies.delete(deleting.value.id)
    appStore.showSuccess(t('admin.routingStrategies.deleted'))
    showDeleteDialog.value = false
    deleting.value = null
    await loadStrategies()
  } catch (error: any) {
    console.error('Failed to delete routing strategy:', error)
    appStore.showError(error.response?.data?.detail || t('admin.routingStrategies.saveFailed'))
  }
}

// ===== Tester =====
const showTester = ref(false)
const testing = ref(false)
const testResult = ref<TestRoutingStrategyResult | null>(null)
const testForm = reactive<{
  platform: string
  group_id: number | null
  model: string
  client: string
  user_agent: string
}>({
  platform: 'anthropic',
  group_id: null,
  model: '',
  client: 'claude_code',
  user_agent: ''
})

function openTester() {
  testResult.value = null
  showTester.value = true
}

async function runTest() {
  testing.value = true
  try {
    testResult.value = await adminAPI.routingStrategies.test({
      platform: testForm.platform,
      group_id: testForm.group_id,
      model: testForm.model.trim(),
      client: testForm.client,
      user_agent: testForm.user_agent.trim()
    })
  } catch (error: any) {
    console.error('Failed to test routing strategy:', error)
    appStore.showError(error.response?.data?.detail || t('admin.routingStrategies.loadFailed'))
  } finally {
    testing.value = false
  }
}

onMounted(async () => {
  await Promise.all([loadGroups(), loadAccounts()])
  await loadStrategies()
})
</script>
