<template>
  <AppLayout>
    <div class="space-y-6">
      <div>
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
          {{ t("admin.settings.errorHandlingRule.title") }}
        </h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t("admin.settings.errorHandlingRule.description") }}
        </p>
      </div>

      <div class="card" data-testid="error-handling-rule-card">
        <div class="space-y-5 p-6">
          <div
            v-if="errorHandlingRuleLoading"
            class="flex items-center gap-2 text-gray-500"
          >
            <div
              class="h-4 w-4 animate-spin rounded-full border-b-2 border-primary-600"
            ></div>
            {{ t("common.loading") }}
          </div>

          <template v-else>
            <div class="flex items-center justify-between">
              <div>
                <label class="font-medium text-gray-900 dark:text-white">
                  {{ t("admin.settings.errorHandlingRule.enabled") }}
                </label>
                <p class="text-sm text-gray-500 dark:text-gray-400">
                  {{ t("admin.settings.errorHandlingRule.enabledHint") }}
                </p>
              </div>
              <Toggle v-model="errorHandlingRuleForm.enabled" />
            </div>

            <!-- 规则表格照常显示，不受总开关影响：总开关关掉时恰恰更需要确认配置还在 -->
            <div
              class="space-y-4 border-t border-gray-100 pt-4 dark:border-dark-700"
            >
              <div class="flex flex-wrap items-center gap-3">
                <label
                  class="text-sm font-medium text-gray-700 dark:text-gray-300"
                >
                  {{ t("admin.settings.errorHandlingRule.defaultRetryCount") }}
                </label>
                <input
                  v-model.number="errorHandlingRuleForm.default_retry_count"
                  type="number"
                  min="0"
                  :max="ERROR_HANDLING_RULE_MAX_RETRY"
                  class="input input-sm w-24"
                  data-testid="error-handling-rule-default-retry"
                />
                <p class="text-xs text-gray-500 dark:text-gray-400">
                  {{
                    t("admin.settings.errorHandlingRule.defaultRetryCountHint", {
                      max: ERROR_HANDLING_RULE_MAX_RETRY,
                    })
                  }}
                </p>
              </div>

              <div class="flex flex-wrap items-center justify-between gap-3">
                <p class="text-xs text-gray-500 dark:text-gray-400">
                  {{ t("admin.settings.errorHandlingRule.orderHint") }}
                </p>
                <button
                  type="button"
                  data-testid="error-handling-rule-add"
                  class="btn btn-primary btn-sm"
                  :disabled="
                    errorHandlingRuleForm.rules.length >=
                    ERROR_HANDLING_RULE_MAX_RULES
                  "
                  :title="t('admin.settings.errorHandlingRule.addRule')"
                  @click="openCreateRuleDialog"
                >
                  <Icon name="plus" size="sm" class="mr-1" />
                  {{ t("admin.settings.errorHandlingRule.addRule") }}
                </button>
              </div>

              <!-- 行序即匹配优先级，所以没有任何一列可排序：一旦允许排序，视图顺序
                   会和 rules 数组脱钩，↑↓ 改的数组和用户看到的行就对不上了 -->
              <div class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-700">
                <DataTable
                  :columns="errorHandlingRuleColumns"
                  :data="errorHandlingRuleForm.rules"
                  :row-key="(row: ErrorHandlingRuleFormItem) => row.id"
                  :actions-count="4"
                >
                  <template #cell-priority="{ row }">
                    <span
                      class="inline-flex h-6 min-w-6 items-center justify-center rounded bg-gray-100 px-1.5 text-xs font-medium text-gray-700 dark:bg-dark-600 dark:text-gray-300"
                    >
                      {{ ruleIndex(row) + 1 }}
                    </span>
                  </template>

                  <template #cell-name="{ row }">
                    <div class="min-w-0">
                      <div
                        v-if="row.name"
                        class="truncate font-medium text-gray-900 dark:text-white"
                      >
                        {{ row.name }}
                      </div>
                      <div v-else class="truncate text-gray-400 dark:text-dark-500">
                        {{ t("admin.settings.errorHandlingRule.unnamedRule") }}
                      </div>
                    </div>
                  </template>

                  <template #cell-matcher="{ row }">
                    <div class="space-y-1">
                      <div class="flex flex-wrap items-center gap-1">
                        <span
                          v-for="code in row.status_codes"
                          :key="code"
                          class="badge badge-danger text-xs"
                        >
                          {{ code }}
                        </span>
                        <span
                          v-if="row.status_codes.length === 0"
                          class="text-xs text-gray-400 dark:text-dark-500"
                        >
                          {{ t("admin.settings.errorHandlingRule.anyStatusCode") }}
                        </span>
                      </div>
                      <div class="flex flex-wrap items-center gap-1">
                        <span
                          v-for="keyword in row.keywords.slice(0, 2)"
                          :key="keyword"
                          class="badge badge-gray max-w-48 truncate text-xs"
                          :title="keyword"
                        >
                          "{{ keyword }}"
                        </span>
                        <span
                          v-if="row.keywords.length > 2"
                          class="text-xs text-gray-500 dark:text-gray-400"
                        >
                          {{
                            t("admin.settings.errorHandlingRule.keywordsMore", {
                              count: row.keywords.length - 2,
                            })
                          }}
                        </span>
                        <span
                          v-if="row.keywords.length === 0"
                          class="text-xs text-gray-400 dark:text-dark-500"
                        >
                          {{ t("admin.settings.errorHandlingRule.anyKeyword") }}
                        </span>
                      </div>
                    </div>
                  </template>

                  <template #cell-action="{ row }">
                    <span :class="['badge', ruleActionBadgeClass(row.action)]">
                      {{ ruleActionLabel(row.action) }}
                    </span>
                  </template>

                  <template #cell-retry="{ row }">
                    <span
                      v-if="row.action !== 'retry'"
                      class="text-xs text-gray-400 dark:text-dark-500"
                    >
                      —
                    </span>
                    <span
                      v-else-if="row.retry_count === null"
                      class="text-sm text-gray-500 dark:text-gray-400"
                    >
                      {{
                        t("admin.settings.errorHandlingRule.retryCountDefault", {
                          count: errorHandlingRuleForm.default_retry_count,
                        })
                      }}
                    </span>
                    <span v-else class="text-sm text-gray-900 dark:text-gray-100">
                      {{ row.retry_count }}
                    </span>
                  </template>

                  <template #cell-exhausted="{ row }">
                    <span
                      v-if="row.action === 'passthrough'"
                      class="text-xs text-gray-400 dark:text-dark-500"
                    >
                      —
                    </span>
                    <span v-else class="text-sm text-gray-600 dark:text-gray-300">
                      {{ ruleExhaustedActionLabel(row.exhausted_action) }}
                    </span>
                  </template>

                  <template #cell-enabled="{ row }">
                    <div class="flex items-center gap-2">
                      <Toggle
                        v-model="row.enabled"
                        :data-testid="`error-handling-rule-toggle-${row.id}`"
                      />
                      <span class="sr-only">
                        {{
                          row.enabled
                            ? t("admin.settings.errorHandlingRule.ruleEnabled")
                            : t("admin.settings.errorHandlingRule.ruleDisabled")
                        }}
                      </span>
                    </div>
                  </template>

                  <template #cell-actions="{ row }">
                    <div class="flex items-center justify-end gap-1">
                      <button
                        type="button"
                        :disabled="ruleIndex(row) === 0"
                        class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 disabled:cursor-not-allowed disabled:opacity-40 dark:hover:bg-dark-600 dark:hover:text-gray-300"
                        :title="t('admin.settings.errorHandlingRule.moveUp')"
                        :aria-label="t('admin.settings.errorHandlingRule.moveUp')"
                        @click="moveErrorHandlingRule(ruleIndex(row), -1)"
                      >
                        <Icon name="arrowUp" size="sm" />
                      </button>
                      <button
                        type="button"
                        :disabled="
                          ruleIndex(row) === errorHandlingRuleForm.rules.length - 1
                        "
                        class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 disabled:cursor-not-allowed disabled:opacity-40 dark:hover:bg-dark-600 dark:hover:text-gray-300"
                        :title="t('admin.settings.errorHandlingRule.moveDown')"
                        :aria-label="t('admin.settings.errorHandlingRule.moveDown')"
                        @click="moveErrorHandlingRule(ruleIndex(row), 1)"
                      >
                        <Icon name="arrowDown" size="sm" />
                      </button>
                      <button
                        type="button"
                        class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-300"
                        :title="t('common.edit')"
                        :aria-label="t('common.edit')"
                        @click="openEditRuleDialog(row)"
                      >
                        <Icon name="edit" size="sm" />
                      </button>
                      <button
                        type="button"
                        class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
                        :title="t('common.delete')"
                        :aria-label="t('common.delete')"
                        @click="requestDeleteRule(row)"
                      >
                        <Icon name="trash" size="sm" />
                      </button>
                    </div>
                  </template>

                  <template #empty>
                    <EmptyState
                      :title="t('admin.settings.errorHandlingRule.noRules')"
                      :action-text="t('admin.settings.errorHandlingRule.addRule')"
                      @action="openCreateRuleDialog"
                    />
                  </template>
                </DataTable>
              </div>
            </div>

            <div
              class="flex justify-end border-t border-gray-100 pt-4 dark:border-dark-700"
            >
              <button
                type="button"
                data-testid="error-handling-rule-save"
                :disabled="errorHandlingRuleSaving"
                class="btn btn-primary btn-sm"
                @click="saveErrorHandlingRuleSettings"
              >
                {{
                  errorHandlingRuleSaving ? t("common.saving") : t("common.save")
                }}
              </button>
            </div>
          </template>
        </div>
      </div>
    </div>

    <!-- 新增 / 编辑弹窗 -->
    <BaseDialog
      :show="ruleDialogVisible"
      :title="
        ruleDialogEditingId
          ? t('admin.settings.errorHandlingRule.editRule')
          : t('admin.settings.errorHandlingRule.addRule')
      "
      width="wide"
      @close="closeRuleDialog"
    >
      <form
        id="error-handling-rule-form"
        class="space-y-4"
        @submit.prevent="confirmRuleDialog"
      >
        <div>
          <label class="input-label">
            {{ t("admin.settings.errorHandlingRule.name") }}
          </label>
          <input
            v-model="ruleDialogForm.name"
            type="text"
            class="input"
            :placeholder="t('admin.settings.errorHandlingRule.namePlaceholder')"
            data-testid="error-handling-rule-dialog-name"
          />
        </div>

        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div>
            <label class="input-label">
              {{ t("admin.settings.errorHandlingRule.statusCodes") }}
            </label>
            <input
              v-model="ruleDialogForm.status_codes_text"
              type="text"
              class="input"
              placeholder="400, 422"
              data-testid="error-handling-rule-status-codes"
            />
          </div>
          <div>
            <label class="input-label">
              {{ t("admin.settings.errorHandlingRule.action") }}
            </label>
            <select
              v-model="ruleDialogForm.action"
              class="input"
              data-testid="error-handling-rule-action"
            >
              <option value="retry">
                {{ t("admin.settings.errorHandlingRule.actionRetry") }}
              </option>
              <option value="failover">
                {{ t("admin.settings.errorHandlingRule.actionFailover") }}
              </option>
              <option value="passthrough">
                {{ t("admin.settings.errorHandlingRule.actionPassthrough") }}
              </option>
            </select>
            <p
              v-if="ruleDialogForm.action === 'passthrough'"
              class="mt-1 text-xs text-amber-600 dark:text-amber-400"
            >
              {{ t("admin.settings.errorHandlingRule.passthroughWarning") }}
            </p>
          </div>
        </div>

        <div>
          <label class="input-label">
            {{ t("admin.settings.errorHandlingRule.keywords") }}
          </label>
          <textarea
            v-model="ruleDialogForm.keywords_text"
            rows="3"
            class="input w-full resize-y text-sm"
            :placeholder="
              t('admin.settings.errorHandlingRule.keywordsPlaceholder')
            "
            data-testid="error-handling-rule-keywords"
          ></textarea>
        </div>

        <div v-if="ruleDialogForm.action === 'retry'">
          <label class="input-label">
            {{ t("admin.settings.errorHandlingRule.retryCount") }}
          </label>
          <input
            :value="ruleDialogForm.retry_count ?? ''"
            type="number"
            min="0"
            :max="ERROR_HANDLING_RULE_MAX_RETRY"
            class="input w-32"
            :placeholder="
              t('admin.settings.errorHandlingRule.retryCountPlaceholder')
            "
            data-testid="error-handling-rule-retry-count"
            @change="
              setDialogRetryCount(($event.target as HTMLInputElement).value)
            "
          />
        </div>
        <p v-else class="text-xs text-gray-400 dark:text-gray-500">
          {{ t("admin.settings.errorHandlingRule.noRetryForAction") }}
        </p>

        <div v-if="ruleDialogForm.action !== 'passthrough'">
          <label class="input-label">
            {{ t("admin.settings.errorHandlingRule.exhaustedAction") }}
          </label>
          <select
            v-model="ruleDialogForm.exhausted_action"
            class="input sm:w-72"
            data-testid="error-handling-rule-exhausted-action"
          >
            <option value="default">
              {{ t("admin.settings.errorHandlingRule.exhaustedActionDefault") }}
            </option>
            <option value="passthrough">
              {{
                t("admin.settings.errorHandlingRule.exhaustedActionPassthrough")
              }}
            </option>
          </select>
          <p
            v-if="ruleDialogForm.exhausted_action === 'passthrough'"
            class="mt-1 text-xs text-amber-600 dark:text-amber-400"
          >
            {{ t("admin.settings.errorHandlingRule.exhaustedActionWarning") }}
          </p>
        </div>

        <p
          v-if="ruleDialogError"
          class="text-sm text-red-600 dark:text-red-400"
          data-testid="error-handling-rule-dialog-error"
        >
          {{ ruleDialogError }}
        </p>
      </form>

      <template #footer>
        <div class="grid w-full grid-cols-2 gap-2 sm:flex sm:justify-end sm:gap-3">
          <button
            type="button"
            class="btn btn-secondary w-full sm:w-auto"
            @click="closeRuleDialog"
          >
            {{ t("common.cancel") }}
          </button>
          <button
            type="submit"
            form="error-handling-rule-form"
            class="btn btn-primary w-full sm:w-auto"
            data-testid="error-handling-rule-dialog-confirm"
          >
            {{ t("common.confirm") }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="deleteDialogVisible"
      :title="t('admin.settings.errorHandlingRule.deleteConfirmTitle')"
      :message="deleteConfirmMessage"
      danger
      @confirm="confirmDeleteRule"
      @cancel="cancelDeleteRule"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";

import AppLayout from "@/components/layout/AppLayout.vue";
import Icon from "@/components/icons/Icon.vue";
import Toggle from "@/components/common/Toggle.vue";
import DataTable from "@/components/common/DataTable.vue";
import BaseDialog from "@/components/common/BaseDialog.vue";
import ConfirmDialog from "@/components/common/ConfirmDialog.vue";
import EmptyState from "@/components/common/EmptyState.vue";
import type { Column } from "@/components/common/types";
import { adminAPI } from "@/api";
import type {
  ErrorHandlingRule,
  ErrorHandlingRuleAction,
  ErrorHandlingRuleExhaustedAction,
  ErrorHandlingRuleSettings,
} from "@/api/admin/settings";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";

const { t } = useI18n();
const appStore = useAppStore();

// 与后端 errorHandlingRuleMaxRetryCount / errorHandlingRuleMaxRules 保持一致
const ERROR_HANDLING_RULE_MAX_RETRY = 4;
const ERROR_HANDLING_RULE_MAX_RULES = 50;
const ERROR_HANDLING_RULE_MIN_STATUS_CODE = 100;
const ERROR_HANDLING_RULE_MAX_STATUS_CODE = 599;

type ErrorHandlingRuleFormItem = {
  /** 稳定标识：新增时前端就生成，兼作表格 row key 和后端 rule id */
  id: string;
  name: string;
  enabled: boolean;
  status_codes: number[];
  keywords: string[];
  action: ErrorHandlingRuleAction;
  retry_count: number | null;
  exhausted_action: ErrorHandlingRuleExhaustedAction;
};

/** 弹窗里编辑的是原始文本，确定时才解析，避免输入过程中被重排 */
type ErrorHandlingRuleDialogForm = {
  name: string;
  status_codes_text: string;
  keywords_text: string;
  action: ErrorHandlingRuleAction;
  retry_count: number | null;
  exhausted_action: ErrorHandlingRuleExhaustedAction;
};

let errorHandlingRuleIDSequence = 0;

/**
 * 规则 ID 在前端就定下来，而不是提交空串让后端补：ID 是
 * errorHandlingRuleTracker 记重试计数的键，前端能稳定持有它，上下移动、
 * 反复保存都不会让同一条规则换 ID。
 */
function nextErrorHandlingRuleID(): string {
  const generated = globalThis.crypto?.randomUUID?.();
  if (generated) return generated;
  errorHandlingRuleIDSequence += 1;
  return `error-handling-rule-${Date.now()}-${errorHandlingRuleIDSequence}`;
}

const errorHandlingRuleLoading = ref(true);
const errorHandlingRuleSaving = ref(false);
const errorHandlingRuleForm = reactive({
  enabled: false,
  default_retry_count: 1,
  rules: [] as ErrorHandlingRuleFormItem[],
});

// 没有任何一列设 sortable：行序即匹配优先级，可排序会让视图顺序和数组脱钩
const errorHandlingRuleColumns = computed<Column[]>(() => [
  { key: "priority", label: t("admin.settings.errorHandlingRule.priority") },
  { key: "name", label: t("admin.settings.errorHandlingRule.name") },
  { key: "matcher", label: t("admin.settings.errorHandlingRule.matcher") },
  { key: "action", label: t("admin.settings.errorHandlingRule.action") },
  {
    key: "retry",
    label: t("admin.settings.errorHandlingRule.retryCountColumn"),
  },
  {
    key: "exhausted",
    label: t("admin.settings.errorHandlingRule.exhaustedAction"),
  },
  { key: "enabled", label: t("admin.settings.errorHandlingRule.ruleStatus") },
  { key: "actions", label: t("common.actions") },
]);

function ruleIndex(rule: ErrorHandlingRuleFormItem): number {
  return errorHandlingRuleForm.rules.findIndex((item) => item.id === rule.id);
}

function ruleActionLabel(action: ErrorHandlingRuleAction): string {
  switch (action) {
    case "failover":
      return t("admin.settings.errorHandlingRule.actionFailover");
    case "passthrough":
      return t("admin.settings.errorHandlingRule.actionPassthrough");
    default:
      return t("admin.settings.errorHandlingRule.actionRetry");
  }
}

function ruleActionBadgeClass(action: ErrorHandlingRuleAction): string {
  switch (action) {
    case "failover":
      return "badge-warning";
    case "passthrough":
      return "badge-gray";
    default:
      return "badge-primary";
  }
}

function ruleExhaustedActionLabel(
  action: ErrorHandlingRuleExhaustedAction,
): string {
  return action === "passthrough"
    ? t("admin.settings.errorHandlingRule.exhaustedActionPassthrough")
    : t("admin.settings.errorHandlingRule.exhaustedActionDefault");
}

/**
 * 严格解析状态码列表：只接受十进制整数，且必须落在 100–599。
 * 任何非法 token 都会被收集起来，由调用方提示用户，而不是静默丢弃。
 */
function parseStatusCodes(raw: string): {
  codes: number[];
  invalid: string[];
} {
  const codes: number[] = [];
  const invalid: string[] = [];
  for (const token of raw.split(/[\s,]+/)) {
    const value = token.trim();
    if (value === "") continue;
    if (!/^\d+$/.test(value)) {
      invalid.push(value);
      continue;
    }
    const parsed = Number.parseInt(value, 10);
    if (
      parsed < ERROR_HANDLING_RULE_MIN_STATUS_CODE ||
      parsed > ERROR_HANDLING_RULE_MAX_STATUS_CODE
    ) {
      invalid.push(value);
      continue;
    }
    codes.push(parsed);
  }
  return { codes, invalid };
}

function parseKeywordList(raw: string): string[] {
  return raw
    .split("\n")
    .map((value) => value.trim())
    .filter((value) => value !== "");
}

/** 保证发给后端的重试次数一定是 0–{ERROR_HANDLING_RULE_MAX_RETRY} 的整数 */
function clampErrorHandlingRetryCount(value: unknown): number {
  const parsed =
    typeof value === "number" ? value : Number.parseInt(String(value ?? ""), 10);
  if (!Number.isFinite(parsed)) return 0;
  return Math.min(
    ERROR_HANDLING_RULE_MAX_RETRY,
    Math.max(0, Math.trunc(parsed)),
  );
}

function moveErrorHandlingRule(index: number, delta: number) {
  const target = index + delta;
  if (index < 0 || target < 0 || target >= errorHandlingRuleForm.rules.length) {
    return;
  }
  const rules = errorHandlingRuleForm.rules;
  [rules[index], rules[target]] = [rules[target], rules[index]];
}

function toErrorHandlingRuleFormItems(
  rules: ErrorHandlingRule[] | undefined,
): ErrorHandlingRuleFormItem[] {
  return (rules || []).map((rule) => ({
    id: rule.id || nextErrorHandlingRuleID(),
    name: rule.name,
    // 后端 null/undefined 表示存量规则，按启用处理
    enabled: rule.enabled ?? true,
    status_codes: rule.status_codes || [],
    keywords: rule.keywords || [],
    action: rule.action || "retry",
    retry_count: rule.retry_count ?? null,
    exhausted_action: rule.exhausted_action || "default",
  }));
}

// ==================== 新增 / 编辑弹窗 ====================

const ruleDialogVisible = ref(false);
const ruleDialogEditingId = ref<string | null>(null);
const ruleDialogError = ref("");
const ruleDialogForm = reactive<ErrorHandlingRuleDialogForm>({
  name: "",
  status_codes_text: "",
  keywords_text: "",
  action: "retry",
  retry_count: null,
  exhausted_action: "default",
});

function setDialogRetryCount(raw: string) {
  const trimmed = raw.trim();
  if (trimmed === "") {
    ruleDialogForm.retry_count = null;
    return;
  }
  const parsed = parseInt(trimmed, 10);
  ruleDialogForm.retry_count = Number.isNaN(parsed)
    ? null
    : clampErrorHandlingRetryCount(parsed);
}

function openCreateRuleDialog() {
  if (errorHandlingRuleForm.rules.length >= ERROR_HANDLING_RULE_MAX_RULES) {
    appStore.showError(
      t("admin.settings.errorHandlingRule.maxRulesReached", {
        max: ERROR_HANDLING_RULE_MAX_RULES,
      }),
    );
    return;
  }
  ruleDialogEditingId.value = null;
  ruleDialogError.value = "";
  Object.assign(ruleDialogForm, {
    name: "",
    status_codes_text: "",
    keywords_text: "",
    action: "retry" as ErrorHandlingRuleAction,
    retry_count: null,
    exhausted_action: "default" as ErrorHandlingRuleExhaustedAction,
  });
  ruleDialogVisible.value = true;
}

function openEditRuleDialog(rule: ErrorHandlingRuleFormItem) {
  ruleDialogEditingId.value = rule.id;
  ruleDialogError.value = "";
  Object.assign(ruleDialogForm, {
    name: rule.name,
    status_codes_text: rule.status_codes.join(", "),
    keywords_text: rule.keywords.join("\n"),
    action: rule.action,
    retry_count: rule.retry_count,
    exhausted_action: rule.exhausted_action,
  });
  ruleDialogVisible.value = true;
}

function closeRuleDialog() {
  ruleDialogVisible.value = false;
  ruleDialogEditingId.value = null;
  ruleDialogError.value = "";
}

/** 单条校验：失败时就地提示且不关闭弹窗，避免用户丢掉已填内容 */
function confirmRuleDialog() {
  const { codes, invalid } = parseStatusCodes(ruleDialogForm.status_codes_text);
  if (invalid.length > 0) {
    ruleDialogError.value = t(
      "admin.settings.errorHandlingRule.invalidStatusCode",
      { value: invalid.join(", ") },
    );
    return;
  }
  const keywords = parseKeywordList(ruleDialogForm.keywords_text);
  if (codes.length === 0 && keywords.length === 0) {
    ruleDialogError.value = t("admin.settings.errorHandlingRule.emptyMatcher");
    return;
  }

  const isRetry = ruleDialogForm.action === "retry";
  const changes = {
    name: ruleDialogForm.name.trim(),
    status_codes: codes,
    keywords,
    action: ruleDialogForm.action,
    // 后端对非 retry 动作会清掉 retry_count，前端提前对齐，避免表格显示出入
    retry_count:
      isRetry && ruleDialogForm.retry_count !== null
        ? clampErrorHandlingRetryCount(ruleDialogForm.retry_count)
        : null,
    exhausted_action:
      ruleDialogForm.action === "passthrough"
        ? ("default" as ErrorHandlingRuleExhaustedAction)
        : ruleDialogForm.exhausted_action,
  };

  if (ruleDialogEditingId.value) {
    const existing = errorHandlingRuleForm.rules.find(
      (item) => item.id === ruleDialogEditingId.value,
    );
    if (existing) Object.assign(existing, changes);
  } else {
    errorHandlingRuleForm.rules.push({
      id: nextErrorHandlingRuleID(),
      enabled: true,
      ...changes,
    });
  }
  closeRuleDialog();
}

// ==================== 删除确认 ====================

const deleteDialogVisible = ref(false);
const deleteTargetId = ref<string | null>(null);

const deleteConfirmMessage = computed(() => {
  const target = errorHandlingRuleForm.rules.find(
    (item) => item.id === deleteTargetId.value,
  );
  return t("admin.settings.errorHandlingRule.deleteConfirmMessage", {
    name: target?.name || t("admin.settings.errorHandlingRule.unnamedRule"),
  });
});

function requestDeleteRule(rule: ErrorHandlingRuleFormItem) {
  deleteTargetId.value = rule.id;
  deleteDialogVisible.value = true;
}

function cancelDeleteRule() {
  deleteDialogVisible.value = false;
  deleteTargetId.value = null;
}

function confirmDeleteRule() {
  const index = errorHandlingRuleForm.rules.findIndex(
    (item) => item.id === deleteTargetId.value,
  );
  if (index >= 0) errorHandlingRuleForm.rules.splice(index, 1);
  cancelDeleteRule();
}

// ==================== 加载 / 保存 ====================

/** 表格里的改动只存本地，点「保存」才整体 PUT（后端也只有整体 PUT 接口） */
function buildErrorHandlingRulePayload(): ErrorHandlingRuleSettings {
  const rules: ErrorHandlingRule[] = errorHandlingRuleForm.rules.map((rule) => ({
    id: rule.id,
    name: rule.name,
    enabled: rule.enabled,
    status_codes: rule.status_codes,
    keywords: rule.keywords,
    action: rule.action,
    retry_count:
      rule.retry_count === null
        ? null
        : clampErrorHandlingRetryCount(rule.retry_count),
    exhausted_action:
      rule.action === "passthrough" ? "default" : rule.exhausted_action,
  }));

  // 输入框可能被清空（v-model.number 会给出空字符串），统一收敛成合法整数
  const defaultRetryCount = clampErrorHandlingRetryCount(
    errorHandlingRuleForm.default_retry_count,
  );
  errorHandlingRuleForm.default_retry_count = defaultRetryCount;

  return {
    enabled: errorHandlingRuleForm.enabled,
    default_retry_count: defaultRetryCount,
    rules,
  };
}

async function loadErrorHandlingRuleSettings() {
  errorHandlingRuleLoading.value = true;
  try {
    const settings = await adminAPI.settings.getErrorHandlingRuleSettings();
    errorHandlingRuleForm.enabled = settings.enabled;
    errorHandlingRuleForm.default_retry_count = settings.default_retry_count;
    errorHandlingRuleForm.rules = toErrorHandlingRuleFormItems(settings.rules);
  } catch (_error: unknown) {
    // Silent fail - settings will use defaults
  } finally {
    errorHandlingRuleLoading.value = false;
  }
}

async function saveErrorHandlingRuleSettings() {
  const payload = buildErrorHandlingRulePayload();

  errorHandlingRuleSaving.value = true;
  try {
    const updated =
      await adminAPI.settings.updateErrorHandlingRuleSettings(payload);
    errorHandlingRuleForm.enabled = updated.enabled;
    errorHandlingRuleForm.default_retry_count = updated.default_retry_count;
    errorHandlingRuleForm.rules = toErrorHandlingRuleFormItems(updated.rules);
    appStore.showSuccess(t("admin.settings.errorHandlingRule.saved"));
  } catch (error: unknown) {
    appStore.showError(
      extractApiErrorMessage(
        error,
        t("admin.settings.errorHandlingRule.saveFailed"),
      ),
    );
  } finally {
    errorHandlingRuleSaving.value = false;
  }
}

onMounted(() => {
  loadErrorHandlingRuleSettings();
});
</script>
