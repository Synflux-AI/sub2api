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

              <!-- 行序完全由 priority 决定，所以没有任何一列可排序：一旦允许排序，
                   视图顺序就和优先级脱钩，用户看到的行序不再是真实匹配顺序 -->
              <div class="overflow-hidden rounded-lg border border-gray-200 dark:border-dark-700">
                <DataTable
                  :columns="errorHandlingRuleColumns"
                  :data="sortedRules"
                  :row-key="(row: ErrorHandlingRuleFormItem) => row.id"
                  :actions-count="2"
                >
                  <template #cell-priority="{ row }">
                    <!-- 刻意用 :value + @change 而不是 v-model：v-model 会在打字过程中
                         实时重排，输入 "12" 时敲完 "1" 那行就先跳走了 -->
                    <input
                      :value="row.priority"
                      type="number"
                      :min="ERROR_HANDLING_RULE_MIN_PRIORITY"
                      :max="ERROR_HANDLING_RULE_MAX_PRIORITY"
                      class="input input-sm w-16"
                      :aria-label="t('admin.settings.errorHandlingRule.priority')"
                      :title="t('admin.settings.errorHandlingRule.priorityHint')"
                      :data-testid="`error-handling-rule-priority-${row.id}`"
                      @change="
                        setRulePriority(row, $event.target as HTMLInputElement)
                      "
                    />
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
                        <!-- 折叠掉的关键字只在弹窗里才能看到，所以计数本身做成悬浮气泡，
                             给出完整清单。HelpTooltip 会 Teleport 到 body，不会被表格
                             那层 overflow-x:auto 的横滚容器裁掉 -->
                        <HelpTooltip
                          v-if="row.keywords.length > 2"
                          width-class="w-72"
                        >
                          <template #trigger>
                            <span
                              class="cursor-help text-xs text-gray-500 underline decoration-dotted underline-offset-2 dark:text-gray-400"
                            >
                              {{
                                t(
                                  "admin.settings.errorHandlingRule.keywordsMore",
                                  { count: row.keywords.length - 2 },
                                )
                              }}
                            </span>
                          </template>
                          <p class="mb-1 font-medium">
                            {{
                              t(
                                "admin.settings.errorHandlingRule.keywordsTooltipTitle",
                              )
                            }}
                          </p>
                          <ul class="space-y-0.5">
                            <!-- 关键字没有去重约束，用下标做 key 免得重复值撞车 -->
                            <li
                              v-for="(keyword, index) in row.keywords"
                              :key="`kw-${index}`"
                              class="break-all"
                            >
                              • {{ keyword }}
                            </li>
                          </ul>
                        </HelpTooltip>
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

        <div>
          <label class="input-label">
            {{ t("admin.settings.errorHandlingRule.priority") }}
          </label>
          <!-- 存原始文本而不是 v-model 出来的 number：v-model 在 type=number 上会
               隐式转数字，越界值就没法在确定时原样报错了 -->
          <input
            :value="ruleDialogForm.priority_text"
            type="number"
            :min="ERROR_HANDLING_RULE_MIN_PRIORITY"
            :max="ERROR_HANDLING_RULE_MAX_PRIORITY"
            class="input w-32"
            data-testid="error-handling-rule-dialog-priority"
            @input="
              ruleDialogForm.priority_text = (
                $event.target as HTMLInputElement
              ).value
            "
          />
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.errorHandlingRule.priorityHint") }}
          </p>
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

        <div>
          <label class="input-label">
            {{ t("admin.settings.errorHandlingRule.platforms") }}
          </label>
          <div class="flex flex-wrap gap-3">
            <label
              v-for="platform in ERROR_HANDLING_RULE_PLATFORM_OPTIONS"
              :key="platform.value"
              class="inline-flex items-center gap-1.5"
            >
              <input
                type="checkbox"
                :value="platform.value"
                v-model="ruleDialogForm.platforms"
                class="h-3.5 w-3.5 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                :data-testid="`error-handling-rule-platform-${platform.value}`"
              />
              <span class="text-xs text-gray-700 dark:text-gray-300">
                {{ platform.label }}
              </span>
            </label>
          </div>
          <p class="input-hint text-xs mt-1">
            {{ t("admin.settings.errorHandlingRule.platformsHint") }}
          </p>
        </div>

        <div>
          <label class="input-label">
            {{ t("admin.settings.errorHandlingRule.maxUpstreamLatency") }}
          </label>
          <!-- 刻意不用 v-model：type="number" 上 Vue 会把值自动转成 number，
               而这里要保留「空串 = 不限制」和用户原样输入的文本，确定时才解析。 -->
          <input
            :value="ruleDialogForm.max_upstream_latency_text"
            type="number"
            min="0"
            :max="ERROR_HANDLING_RULE_MAX_UPSTREAM_LATENCY_MS"
            class="input w-40"
            placeholder="0"
            data-testid="error-handling-rule-max-upstream-latency"
            @input="
              ruleDialogForm.max_upstream_latency_text = (
                $event.target as HTMLInputElement
              ).value
            "
          />
          <p class="input-hint text-xs mt-1">
            {{ t("admin.settings.errorHandlingRule.maxUpstreamLatencyHint") }}
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
import HelpTooltip from "@/components/common/HelpTooltip.vue";
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
// 与后端 priority 的合法区间保持一致
const ERROR_HANDLING_RULE_MIN_PRIORITY = 1;
const ERROR_HANDLING_RULE_MAX_PRIORITY = 999;

type ErrorHandlingRuleFormItem = {
  /** 稳定标识：新增时前端就生成，兼作表格 row key 和后端 rule id */
  id: string;
  name: string;
  enabled: boolean;
  /** 匹配优先级，数值越小越先匹配；视图顺序完全由它决定 */
  priority: number;
  status_codes: number[];
  keywords: string[];
  action: ErrorHandlingRuleAction;
  retry_count: number | null;
  exhausted_action: ErrorHandlingRuleExhaustedAction;
  /** 适用平台。「全平台」= 全部勾上；空数组不是合法提交值 */
  platforms: string[];
  /** 上游耗时上限（毫秒），0 = 不限制 */
  max_upstream_latency_ms: number;
};

/** 弹窗里编辑的是原始文本，确定时才解析，避免输入过程中被重排 */
type ErrorHandlingRuleDialogForm = {
  name: string;
  priority_text: string;
  status_codes_text: string;
  keywords_text: string;
  action: ErrorHandlingRuleAction;
  retry_count: number | null;
  exhausted_action: ErrorHandlingRuleExhaustedAction;
  platforms: string[];
  max_upstream_latency_text: string;
};

/**
 * 只列出规则引擎真正接线了的平台。后端 validate 也只放行这两个：勾一个引擎没接线的
 * 平台，管理员会以为规则在跑，实际什么都不会发生。
 */
const ERROR_HANDLING_RULE_PLATFORM_OPTIONS = [
  { value: "anthropic", label: "Anthropic" },
  { value: "openai", label: "OpenAI" },
] as const;

const ERROR_HANDLING_RULE_MAX_UPSTREAM_LATENCY_MS = 3_600_000;

let errorHandlingRuleIDSequence = 0;

/**
 * 规则 ID 在前端就定下来，而不是提交空串让后端补：ID 是
 * errorHandlingRuleTracker 记重试计数的键，前端能稳定持有它，改优先级、
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

/**
 * 视图顺序只由 priority 决定：按升序排，相同 priority 保持数组原有先后
 * （Array.prototype.sort 稳定，但必须先拷贝，不能原地 sort 这个 reactive 数组）。
 * 后端对相同 priority 也是按提交顺序稳定排序，两边口径一致。
 */
const sortedRules = computed<ErrorHandlingRuleFormItem[]>(() =>
  [...errorHandlingRuleForm.rules].sort((a, b) => a.priority - b.priority),
);

// 没有任何一列设 sortable：行序就是匹配优先级，可排序会让视图顺序和 priority 脱钩
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

/**
 * 保证优先级一定是 {ERROR_HANDLING_RULE_MIN_PRIORITY}–{ERROR_HANDLING_RULE_MAX_PRIORITY}
 * 的整数。空值 / 非法输入回落到 fallback（通常是改动前的值），否则清空输入框
 * 就会把整行的优先级变成 NaN，行序直接乱掉。
 */
function clampErrorHandlingPriority(
  value: unknown,
  fallback: number = ERROR_HANDLING_RULE_MIN_PRIORITY,
): number {
  const parsed =
    typeof value === "number" ? value : Number.parseInt(String(value ?? ""), 10);
  const resolved = Number.isFinite(parsed) ? parsed : fallback;
  if (!Number.isFinite(resolved)) return ERROR_HANDLING_RULE_MIN_PRIORITY;
  return Math.min(
    ERROR_HANDLING_RULE_MAX_PRIORITY,
    Math.max(ERROR_HANDLING_RULE_MIN_PRIORITY, Math.trunc(resolved)),
  );
}

/** 新增规则默认排到最末，免得每加一条都要手动改数字才不会插队 */
function nextErrorHandlingRulePriority(): number {
  if (errorHandlingRuleForm.rules.length === 0) {
    return ERROR_HANDLING_RULE_MIN_PRIORITY;
  }
  const max = Math.max(
    ...errorHandlingRuleForm.rules.map((rule) => rule.priority),
  );
  return clampErrorHandlingPriority(max + 1);
}

/**
 * 行内改优先级：失焦 / 回车才提交，提交后该行按新优先级重排。
 * 还要把收敛后的值写回 DOM——模型值没变时（比如把 1 改成 0）Vue 不会重绘，
 * 输入框会一直留着那个非法数字。
 */
function setRulePriority(
  rule: ErrorHandlingRuleFormItem,
  input: HTMLInputElement,
) {
  const target = errorHandlingRuleForm.rules.find(
    (item) => item.id === rule.id,
  );
  if (!target) return;
  target.priority = clampErrorHandlingPriority(input.value, target.priority);
  input.value = String(target.priority);
}

function toErrorHandlingRuleFormItems(
  rules: ErrorHandlingRule[] | undefined,
): ErrorHandlingRuleFormItem[] {
  return (rules || []).map((rule, index) => {
    // 后端 normalize 后一定带合法 priority；缺字段 / <=0 的存量响应按下标补 1..N
    const fallbackPriority = index + 1;
    return {
      id: rule.id || nextErrorHandlingRuleID(),
      name: rule.name,
      // 后端 null/undefined 表示存量规则，按启用处理
      enabled: rule.enabled ?? true,
      priority: clampErrorHandlingPriority(
        rule.priority && rule.priority > 0 ? rule.priority : fallbackPriority,
        fallbackPriority,
      ),
      status_codes: rule.status_codes || [],
      keywords: rule.keywords || [],
      action: rule.action || "retry",
      retry_count: rule.retry_count ?? null,
      exhausted_action: rule.exhausted_action || "default",
      // 后端对存量规则会 normalize 成 ["anthropic"]；这里对齐，避免旧响应显示成「全不选」
      platforms:
        rule.platforms && rule.platforms.length > 0
          ? [...rule.platforms]
          : ["anthropic"],
      max_upstream_latency_ms: normalizeMaxUpstreamLatency(
        rule.max_upstream_latency_ms,
      ),
    };
  });
}

// ==================== 新增 / 编辑弹窗 ====================

const ruleDialogVisible = ref(false);
const ruleDialogEditingId = ref<string | null>(null);
const ruleDialogError = ref("");
const ruleDialogForm = reactive<ErrorHandlingRuleDialogForm>({
  name: "",
  priority_text: String(ERROR_HANDLING_RULE_MIN_PRIORITY),
  status_codes_text: "",
  keywords_text: "",
  action: "retry",
  retry_count: null,
  exhausted_action: "default",
  platforms: ["anthropic"],
  max_upstream_latency_text: "",
});

/** 存量/异常值统一收敛成 0（= 不限制） */
function normalizeMaxUpstreamLatency(value: number | null | undefined): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    return 0;
  }
  return Math.min(
    Math.floor(value),
    ERROR_HANDLING_RULE_MAX_UPSTREAM_LATENCY_MS,
  );
}

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
    priority_text: String(nextErrorHandlingRulePriority()),
    status_codes_text: "",
    keywords_text: "",
    action: "retry" as ErrorHandlingRuleAction,
    retry_count: null,
    exhausted_action: "default" as ErrorHandlingRuleExhaustedAction,
    // 新规则默认只对 Anthropic 生效：与存量语义一致，要扩到 OpenAI 得管理员显式勾
    platforms: ["anthropic"],
    max_upstream_latency_text: "",
  });
  ruleDialogVisible.value = true;
}

function openEditRuleDialog(rule: ErrorHandlingRuleFormItem) {
  ruleDialogEditingId.value = rule.id;
  ruleDialogError.value = "";
  Object.assign(ruleDialogForm, {
    name: rule.name,
    priority_text: String(rule.priority),
    status_codes_text: rule.status_codes.join(", "),
    keywords_text: rule.keywords.join("\n"),
    action: rule.action,
    retry_count: rule.retry_count,
    exhausted_action: rule.exhausted_action,
    platforms: [...rule.platforms],
    max_upstream_latency_text:
      rule.max_upstream_latency_ms > 0
        ? String(rule.max_upstream_latency_ms)
        : "",
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
  // 弹窗里的优先级不静默 clamp：用户明确输了个越界值，就地报错比悄悄改掉更好
  const priorityText = ruleDialogForm.priority_text.trim();
  const priority = /^\d+$/.test(priorityText)
    ? Number.parseInt(priorityText, 10)
    : Number.NaN;
  if (
    !Number.isFinite(priority) ||
    priority < ERROR_HANDLING_RULE_MIN_PRIORITY ||
    priority > ERROR_HANDLING_RULE_MAX_PRIORITY
  ) {
    ruleDialogError.value = t(
      "admin.settings.errorHandlingRule.invalidPriority",
      {
        min: ERROR_HANDLING_RULE_MIN_PRIORITY,
        max: ERROR_HANDLING_RULE_MAX_PRIORITY,
      },
    );
    return;
  }

  const isRetry = ruleDialogForm.action === "retry";
  // 「全平台」= 全部勾上，所以一个都不勾是非法的：空数组到了后端会被 normalize
  // 当成存量配置、静默收窄成 anthropic，与管理员的意图相反。
  const platforms = ERROR_HANDLING_RULE_PLATFORM_OPTIONS.filter((option) =>
    ruleDialogForm.platforms.includes(option.value),
  ).map((option) => option.value as string);
  if (platforms.length === 0) {
    ruleDialogError.value = t(
      "admin.settings.errorHandlingRule.platformsRequired",
    );
    return;
  }
  const latencyText = ruleDialogForm.max_upstream_latency_text.trim();
  const maxUpstreamLatencyMs =
    latencyText === "" ? 0 : Number.parseInt(latencyText, 10);
  if (
    !/^\d*$/.test(latencyText) ||
    !Number.isFinite(maxUpstreamLatencyMs) ||
    maxUpstreamLatencyMs < 0 ||
    maxUpstreamLatencyMs > ERROR_HANDLING_RULE_MAX_UPSTREAM_LATENCY_MS
  ) {
    ruleDialogError.value = t(
      "admin.settings.errorHandlingRule.invalidMaxUpstreamLatency",
      { max: ERROR_HANDLING_RULE_MAX_UPSTREAM_LATENCY_MS },
    );
    return;
  }

  const changes = {
    name: ruleDialogForm.name.trim(),
    priority: clampErrorHandlingPriority(priority),
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
    platforms,
    max_upstream_latency_ms: maxUpstreamLatencyMs,
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
  // 提交排序后的数组：后端对相同 priority 按提交顺序稳定排序，视图顺序即提交顺序
  const rules: ErrorHandlingRule[] = sortedRules.value.map((rule) => ({
    id: rule.id,
    name: rule.name,
    enabled: rule.enabled,
    priority: clampErrorHandlingPriority(rule.priority),
    status_codes: rule.status_codes,
    keywords: rule.keywords,
    action: rule.action,
    retry_count:
      rule.retry_count === null
        ? null
        : clampErrorHandlingRetryCount(rule.retry_count),
    exhausted_action:
      rule.action === "passthrough" ? "default" : rule.exhausted_action,
    // platforms 必须显式发出去，且必须非空：省略这个 key 会被后端当成存量规则。
    // 表格里的规则都经过 toErrorHandlingRuleFormItems，那里已经兜过空值。
    platforms:
      rule.platforms.length > 0 ? [...rule.platforms] : ["anthropic"],
    max_upstream_latency_ms: normalizeMaxUpstreamLatency(
      rule.max_upstream_latency_ms,
    ),
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
