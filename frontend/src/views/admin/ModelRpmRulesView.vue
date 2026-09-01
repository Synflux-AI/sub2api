<template>
  <AppLayout>
    <div class="w-full min-w-0 space-y-6 pb-8">
      <header
        class="page-header mb-0 rounded-3xl bg-white p-5 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700 sm:p-6"
      >
        <h1 class="page-title flex items-center gap-2 text-xl font-black text-gray-900 dark:text-white">
          <span
            class="inline-flex h-8 w-8 items-center justify-center rounded-xl bg-blue-50 text-blue-500 dark:bg-blue-900/30 dark:text-blue-400"
          >
            <Icon name="chart" size="sm" />
          </span>
          {{ t('admin.modelRpmRules.title') }}
        </h1>
        <p class="page-description mt-1.5 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.modelRpmRules.description') }}
        </p>
      </header>

      <TablePageLayout>
        <template #filters>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <p class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.modelRpmRules.allRulesApplyHint') }}
            </p>
            <div class="flex items-center gap-2">
              <button type="button" class="btn btn-secondary" :disabled="loading" @click="reload">
                <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
                {{ t('common.refresh') }}
              </button>
              <button type="button" class="btn btn-primary" @click="openCreateDialog">
                {{ t('admin.modelRpmRules.createButton') }}
              </button>
            </div>
          </div>
        </template>

        <template #table>
          <DataTable :columns="columns" :data="rules" :loading="loading">
            <template #cell-model_pattern="{ row }">
              <code class="rounded bg-gray-100 px-1.5 py-0.5 text-xs dark:bg-dark-700">{{ row.model_pattern }}</code>
            </template>

            <template #cell-scope="{ row }">
              <span class="text-sm text-gray-900 dark:text-gray-100">{{ t('admin.modelRpmRules.scopes.' + row.scope) }}</span>
            </template>

            <template #cell-target="{ row }">
              <span class="text-sm text-gray-900 dark:text-gray-100">{{ targetLabel(row) }}</span>
            </template>

            <template #cell-rpm_limit="{ row }">
              <span class="text-sm font-medium text-gray-900 dark:text-gray-100">{{ row.rpm_limit }}</span>
            </template>

            <template #cell-enabled="{ row }">
              <Toggle :modelValue="row.enabled" @update:modelValue="toggleEnabled(row)" />
            </template>

            <template #cell-actions="{ row }">
              <div class="flex items-center gap-2">
                <button type="button" class="btn btn-sm btn-secondary" @click="openEditDialog(row)">
                  {{ t('common.edit') }}
                </button>
                <button type="button" class="btn btn-sm btn-danger" @click="handleDelete(row)">
                  {{ t('common.delete') }}
                </button>
              </div>
            </template>

            <template #empty>
              <EmptyState
                :title="t('admin.modelRpmRules.noRulesYet')"
                :description="t('admin.modelRpmRules.createFirstRule')"
                :action-text="t('admin.modelRpmRules.createButton')"
                @action="openCreateDialog"
              />
            </template>
          </DataTable>
        </template>
      </TablePageLayout>
    </div>

    <ModelRpmRuleModal
      :show="showDialog"
      :rule="editing"
      :groups="groups"
      @close="closeDialog"
      @saved="reload"
    />

    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('common.delete')"
      :message="deleteConfirmMessage"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { extractApiErrorMessage } from '@/utils/apiError'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Toggle from '@/components/common/Toggle.vue'
import Icon from '@/components/icons/Icon.vue'
import ModelRpmRuleModal from '@/components/admin/modelRpm/ModelRpmRuleModal.vue'
import type { Column } from '@/components/common/types'
import type { Group, ModelRPMRule } from '@/types'

const { t } = useI18n()
const appStore = useAppStore()

const rules = ref<ModelRPMRule[]>([])
const groups = ref<Group[]>([])
const loading = ref(false)
const showDialog = ref(false)
const editing = ref<ModelRPMRule | null>(null)
const showDeleteDialog = ref(false)
const deleting = ref<ModelRPMRule | null>(null)

let abortController: AbortController | null = null

const columns = computed<Column[]>(() => [
  { key: 'name', label: t('admin.modelRpmRules.columns.name'), sortable: false },
  { key: 'model_pattern', label: t('admin.modelRpmRules.columns.modelPattern'), sortable: false },
  { key: 'scope', label: t('admin.modelRpmRules.columns.scope'), sortable: false },
  { key: 'target', label: t('admin.modelRpmRules.columns.target'), sortable: false },
  { key: 'rpm_limit', label: t('admin.modelRpmRules.columns.rpmLimit'), sortable: false },
  { key: 'enabled', label: t('admin.modelRpmRules.columns.enabled'), sortable: false },
  { key: 'actions', label: t('admin.modelRpmRules.columns.actions'), sortable: false }
])

const deleteConfirmMessage = computed(() =>
  t('admin.modelRpmRules.deleteConfirm', { name: deleting.value?.name || '' })
)

function targetLabel(rule: ModelRPMRule): string {
  if (rule.target_type === 'all') return t('admin.modelRpmRules.targetTypes.all')
  const label = t('admin.modelRpmRules.targetTypes.' + rule.target_type)
  const name = rule.target_name || `#${rule.target_id ?? ''}`
  return `${label} · ${name}`
}

async function reload() {
  if (abortController) abortController.abort()
  const ctrl = new AbortController()
  abortController = ctrl
  loading.value = true
  try {
    const items = await adminAPI.modelRpmRules.list({ signal: ctrl.signal })
    if (ctrl.signal.aborted || abortController !== ctrl) return
    rules.value = items || []
  } catch (err: unknown) {
    const e = err as { name?: string; code?: string }
    if (e?.name === 'AbortError' || e?.code === 'ERR_CANCELED') return
    appStore.showError(extractApiErrorMessage(err, t('admin.modelRpmRules.loadError')))
  } finally {
    if (abortController === ctrl) {
      loading.value = false
      abortController = null
    }
  }
}

async function loadGroups() {
  try {
    const res = await adminAPI.groups.list(1, 200)
    groups.value = res.items || []
  } catch {
    groups.value = []
  }
}

function openCreateDialog() {
  editing.value = null
  showDialog.value = true
}

function openEditDialog(row: ModelRPMRule) {
  editing.value = row
  showDialog.value = true
}

function closeDialog() {
  showDialog.value = false
  editing.value = null
}

async function toggleEnabled(row: ModelRPMRule) {
  const next = !row.enabled
  try {
    await adminAPI.modelRpmRules.update(row.id, {
      name: row.name,
      model_pattern: row.model_pattern,
      scope: row.scope,
      target_type: row.target_type,
      target_id: row.target_type === 'all' ? null : (row.target_id ?? null),
      rpm_limit: row.rpm_limit,
      enabled: next
    })
    row.enabled = next
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

function handleDelete(row: ModelRPMRule) {
  deleting.value = row
  showDeleteDialog.value = true
}

async function confirmDelete() {
  if (!deleting.value) return
  try {
    await adminAPI.modelRpmRules.delete(deleting.value.id)
    appStore.showSuccess(t('admin.modelRpmRules.deleteSuccess'))
    showDeleteDialog.value = false
    deleting.value = null
    await reload()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('common.error')))
  }
}

onMounted(() => {
  void reload()
  void loadGroups()
})

onUnmounted(() => {
  abortController?.abort()
})
</script>
