<template>
  <BaseDialog
    :show="show"
    :title="rule ? t('admin.modelRpmRules.editTitle') : t('admin.modelRpmRules.createTitle')"
    width="normal"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <!-- 规则名 -->
      <div>
        <label class="label" for="model-rpm-rule-name">{{ t('admin.modelRpmRules.fields.name') }}</label>
        <input
          id="model-rpm-rule-name"
          v-model="form.name"
          type="text"
          autocomplete="off"
          class="input w-full"
          :placeholder="t('admin.modelRpmRules.fields.namePlaceholder')"
        />
      </div>

      <!-- 模型匹配 -->
      <div>
        <label class="label" for="model-rpm-rule-pattern">{{ t('admin.modelRpmRules.fields.modelPattern') }}</label>
        <input
          id="model-rpm-rule-pattern"
          v-model="form.model_pattern"
          type="text"
          autocomplete="off"
          class="input w-full"
          placeholder="claude-opus-*"
        />
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.modelRpmRules.fields.modelPatternHint') }}
        </p>
      </div>

      <!-- 适用范围 -->
      <div>
        <label class="label" for="model-rpm-rule-target-type">{{ t('admin.modelRpmRules.fields.targetType') }}</label>
        <select id="model-rpm-rule-target-type" v-model="form.target_type" class="input w-full" @change="handleTargetTypeChange">
          <option value="all">{{ t('admin.modelRpmRules.targetTypes.all') }}</option>
          <option value="group">{{ t('admin.modelRpmRules.targetTypes.group') }}</option>
          <option value="user">{{ t('admin.modelRpmRules.targetTypes.user') }}</option>
        </select>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.modelRpmRules.fields.targetTypeHint') }}
        </p>
      </div>

      <!-- 分组选择 -->
      <div v-if="form.target_type === 'group'">
        <label class="label" for="model-rpm-rule-group">{{ t('admin.modelRpmRules.fields.targetGroup') }}</label>
        <select id="model-rpm-rule-group" v-model.number="groupSelection" class="input w-full">
          <option :value="0" disabled>{{ t('admin.modelRpmRules.fields.selectGroup') }}</option>
          <option v-for="group in groups" :key="group.id" :value="group.id">
            #{{ group.id }} · {{ group.name }}
          </option>
        </select>
      </div>

      <!-- 用户选择 -->
      <div v-if="form.target_type === 'user'" class="relative">
        <label class="label" for="model-rpm-rule-user">{{ t('admin.modelRpmRules.fields.targetUser') }}</label>
        <input
          id="model-rpm-rule-user"
          v-model="userQuery"
          type="text"
          autocomplete="off"
          class="input w-full"
          :placeholder="t('admin.modelRpmRules.fields.searchUserPlaceholder')"
          @input="handleSearchUsers"
          @focus="showUserDropdown = true"
        />
        <div
          v-if="showUserDropdown && userResults.length > 0"
          class="absolute left-0 right-0 top-full z-10 mt-1 max-h-48 overflow-y-auto rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-500 dark:bg-dark-700"
        >
          <button
            v-for="user in userResults"
            :key="user.id"
            type="button"
            class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm hover:bg-gray-50 dark:hover:bg-dark-600"
            @click="selectUser(user)"
          >
            <span class="text-gray-400">#{{ user.id }}</span>
            <span class="text-gray-900 dark:text-white">{{ user.username || user.email }}</span>
          </button>
        </div>
      </div>

      <!-- 配额口径 -->
      <div>
        <label class="label" for="model-rpm-rule-scope">{{ t('admin.modelRpmRules.fields.scope') }}</label>
        <select id="model-rpm-rule-scope" v-model="form.scope" class="input w-full" :disabled="form.target_type === 'user'">
          <option value="user">{{ t('admin.modelRpmRules.scopes.user') }}</option>
          <option value="global">{{ t('admin.modelRpmRules.scopes.global') }}</option>
        </select>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{
            form.target_type === 'user'
              ? t('admin.modelRpmRules.fields.scopeLockedHint')
              : t('admin.modelRpmRules.fields.scopeHint')
          }}
        </p>
      </div>

      <!-- 限额 -->
      <div>
        <label class="label" for="model-rpm-rule-limit">{{ t('admin.modelRpmRules.fields.rpmLimit') }}</label>
        <input
          id="model-rpm-rule-limit"
          v-model.number="form.rpm_limit"
          type="number"
          min="1"
          step="1"
          autocomplete="off"
          class="hide-spinner input w-full"
          placeholder="10"
        />
        <p class="mt-1 text-xs text-amber-600 dark:text-amber-400">
          {{ t('admin.modelRpmRules.fields.rpmLimitHint') }}
        </p>
      </div>

      <!-- 启用 -->
      <div class="flex items-center gap-3">
        <Toggle v-model="form.enabled" />
        <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.modelRpmRules.fields.enabled') }}</span>
      </div>

      <p v-if="validationError" class="text-sm text-red-600 dark:text-red-400">{{ validationError }}</p>
    </div>

    <template #footer>
      <button type="button" class="btn btn-secondary" @click="emit('close')">
        {{ t('common.cancel') }}
      </button>
      <button type="button" class="btn btn-primary" :disabled="saving" @click="handleSave">
        {{ t('common.save') }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { extractApiErrorMessage } from '@/utils/apiError'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Toggle from '@/components/common/Toggle.vue'
import type { Group, ModelRPMRule, SaveModelRPMRuleRequest, User } from '@/types'

const props = defineProps<{
  show: boolean
  rule: ModelRPMRule | null
  groups: Group[]
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'saved'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

const saving = ref(false)
const validationError = ref('')
const userQuery = ref('')
const userResults = ref<User[]>([])
const showUserDropdown = ref(false)
let userSearchTimer: ReturnType<typeof setTimeout> | null = null

const form = reactive<SaveModelRPMRuleRequest>({
  name: '',
  model_pattern: '',
  scope: 'user',
  target_type: 'all',
  target_id: null,
  rpm_limit: 10,
  enabled: true
})

const groupSelection = computed({
  get: () => form.target_id ?? 0,
  set: (value: number) => {
    form.target_id = value > 0 ? value : null
  }
})

watch(
  () => props.show,
  (show) => {
    if (!show) return
    validationError.value = ''
    userResults.value = []
    showUserDropdown.value = false
    if (props.rule) {
      form.name = props.rule.name
      form.model_pattern = props.rule.model_pattern
      form.scope = props.rule.scope
      form.target_type = props.rule.target_type
      form.target_id = props.rule.target_id ?? null
      form.rpm_limit = props.rule.rpm_limit
      form.enabled = props.rule.enabled
      userQuery.value = props.rule.target_type === 'user' ? props.rule.target_name || '' : ''
    } else {
      form.name = ''
      form.model_pattern = ''
      form.scope = 'user'
      form.target_type = 'all'
      form.target_id = null
      form.rpm_limit = 10
      form.enabled = true
      userQuery.value = ''
    }
  },
  { immediate: true }
)

function handleTargetTypeChange() {
  form.target_id = null
  userQuery.value = ''
  userResults.value = []
  // target_type=user 时两种 scope 效果相同（目标只有一个用户），锁定为 user 避免歧义。
  if (form.target_type === 'user') {
    form.scope = 'user'
  }
}

function handleSearchUsers() {
  if (userSearchTimer) clearTimeout(userSearchTimer)
  form.target_id = null
  userSearchTimer = setTimeout(async () => {
    const query = userQuery.value.trim()
    if (!query) {
      userResults.value = []
      return
    }
    try {
      const res = await adminAPI.users.list(1, 10, { search: query })
      userResults.value = res.items || []
      showUserDropdown.value = true
    } catch {
      userResults.value = []
    }
  }, 300)
}

function selectUser(user: User) {
  form.target_id = user.id
  userQuery.value = user.username || user.email
  showUserDropdown.value = false
}

function validate(): string {
  if (!form.name.trim()) return t('admin.modelRpmRules.errors.nameRequired')
  if (!form.model_pattern.trim()) return t('admin.modelRpmRules.errors.modelPatternRequired')
  const pattern = form.model_pattern.trim()
  const wildcardAt = pattern.indexOf('*')
  if (pattern === '*' || (wildcardAt >= 0 && wildcardAt !== pattern.length - 1)) {
    return t('admin.modelRpmRules.errors.modelPatternWildcard')
  }
  if (form.target_type !== 'all' && !form.target_id) {
    return t('admin.modelRpmRules.errors.targetRequired')
  }
  if (!Number.isInteger(form.rpm_limit) || form.rpm_limit <= 0) {
    return t('admin.modelRpmRules.errors.rpmLimitPositive')
  }
  return ''
}

async function handleSave() {
  const error = validate()
  validationError.value = error
  if (error) return

  const payload: SaveModelRPMRuleRequest = {
    name: form.name.trim(),
    model_pattern: form.model_pattern.trim(),
    scope: form.scope,
    target_type: form.target_type,
    target_id: form.target_type === 'all' ? null : form.target_id,
    rpm_limit: form.rpm_limit,
    enabled: form.enabled
  }

  saving.value = true
  try {
    if (props.rule) {
      await adminAPI.modelRpmRules.update(props.rule.id, payload)
    } else {
      await adminAPI.modelRpmRules.create(payload)
    }
    appStore.showSuccess(t('admin.modelRpmRules.saveSuccess'))
    emit('saved')
    emit('close')
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.modelRpmRules.saveFailed')))
  } finally {
    saving.value = false
  }
}
</script>
