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

            <div
              v-if="errorHandlingRuleForm.enabled"
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

              <div
                v-for="(rule, index) in errorHandlingRuleForm.rules"
                :key="rule.form_key"
                class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600"
              >
                <div class="flex items-center gap-2">
                  <input
                    v-model="rule.name"
                    type="text"
                    class="input input-sm min-w-0 flex-1"
                    :placeholder="
                      t('admin.settings.errorHandlingRule.namePlaceholder')
                    "
                  />
                  <button
                    type="button"
                    :disabled="index === 0"
                    class="btn btn-ghost btn-xs h-7 w-7 p-0"
                    :title="t('admin.settings.errorHandlingRule.moveUp')"
                    :aria-label="t('admin.settings.errorHandlingRule.moveUp')"
                    @click="moveErrorHandlingRule(index, -1)"
                  >
                    <Icon name="arrowUp" size="xs" />
                  </button>
                  <button
                    type="button"
                    :disabled="index === errorHandlingRuleForm.rules.length - 1"
                    class="btn btn-ghost btn-xs h-7 w-7 p-0"
                    :title="t('admin.settings.errorHandlingRule.moveDown')"
                    :aria-label="t('admin.settings.errorHandlingRule.moveDown')"
                    @click="moveErrorHandlingRule(index, 1)"
                  >
                    <Icon name="arrowDown" size="xs" />
                  </button>
                  <button
                    type="button"
                    class="btn btn-ghost btn-xs h-7 w-7 p-0 text-red-500 hover:text-red-700"
                    :title="t('common.delete')"
                    :aria-label="t('common.delete')"
                    @click="errorHandlingRuleForm.rules.splice(index, 1)"
                  >
                    <Icon name="trash" size="xs" />
                  </button>
                </div>

                <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <div>
                    <label
                      class="mb-1 block text-xs text-gray-500 dark:text-gray-400"
                    >
                      {{ t("admin.settings.errorHandlingRule.statusCodes") }}
                    </label>
                    <input
                      v-model="rule.status_codes_text"
                      type="text"
                      class="input input-sm w-full"
                      placeholder="400, 422"
                      data-testid="error-handling-rule-status-codes"
                    />
                  </div>
                  <div>
                    <label
                      class="mb-1 block text-xs text-gray-500 dark:text-gray-400"
                    >
                      {{ t("admin.settings.errorHandlingRule.action") }}
                    </label>
                    <select
                      v-model="rule.action"
                      class="input input-sm w-full"
                      data-testid="error-handling-rule-action"
                    >
                      <option value="retry">
                        {{ t("admin.settings.errorHandlingRule.actionRetry") }}
                      </option>
                      <option value="failover">
                        {{
                          t("admin.settings.errorHandlingRule.actionFailover")
                        }}
                      </option>
                      <option value="passthrough">
                        {{
                          t("admin.settings.errorHandlingRule.actionPassthrough")
                        }}
                      </option>
                    </select>
                    <p
                      v-if="rule.action === 'passthrough'"
                      class="mt-1 text-xs text-amber-600 dark:text-amber-400"
                    >
                      {{
                        t("admin.settings.errorHandlingRule.passthroughWarning")
                      }}
                    </p>
                  </div>
                </div>

                <div>
                  <label
                    class="mb-1 block text-xs text-gray-500 dark:text-gray-400"
                  >
                    {{ t("admin.settings.errorHandlingRule.keywords") }}
                  </label>
                  <textarea
                    v-model="rule.keywords_text"
                    rows="3"
                    class="input w-full resize-y text-sm"
                    :placeholder="
                      t('admin.settings.errorHandlingRule.keywordsPlaceholder')
                    "
                    data-testid="error-handling-rule-keywords"
                  ></textarea>
                </div>

                <div v-if="rule.action === 'retry'">
                  <label
                    class="mb-1 block text-xs text-gray-500 dark:text-gray-400"
                  >
                    {{ t("admin.settings.errorHandlingRule.retryCount") }}
                  </label>
                  <input
                    :value="rule.retry_count ?? ''"
                    type="number"
                    min="0"
                    :max="ERROR_HANDLING_RULE_MAX_RETRY"
                    class="input input-sm w-32"
                    :placeholder="
                      t('admin.settings.errorHandlingRule.retryCountPlaceholder')
                    "
                    @change="
                      setRuleRetryCount(
                        rule,
                        ($event.target as HTMLInputElement).value,
                      )
                    "
                  />
                </div>
                <p v-else class="text-xs text-gray-400 dark:text-gray-500">
                  {{ t("admin.settings.errorHandlingRule.noRetryForAction") }}
                </p>

                <div v-if="rule.action !== 'passthrough'">
                  <label
                    class="mb-1 block text-xs text-gray-500 dark:text-gray-400"
                  >
                    {{ t("admin.settings.errorHandlingRule.exhaustedAction") }}
                  </label>
                  <select
                    v-model="rule.exhausted_action"
                    class="input input-sm w-full sm:w-72"
                    data-testid="error-handling-rule-exhausted-action"
                  >
                    <option value="default">
                      {{
                        t(
                          "admin.settings.errorHandlingRule.exhaustedActionDefault",
                        )
                      }}
                    </option>
                    <option value="passthrough">
                      {{
                        t(
                          "admin.settings.errorHandlingRule.exhaustedActionPassthrough",
                        )
                      }}
                    </option>
                  </select>
                  <p
                    v-if="rule.exhausted_action === 'passthrough'"
                    class="mt-1 text-xs text-amber-600 dark:text-amber-400"
                  >
                    {{
                      t(
                        "admin.settings.errorHandlingRule.exhaustedActionWarning",
                      )
                    }}
                  </p>
                </div>
              </div>

              <button
                type="button"
                data-testid="error-handling-rule-add"
                class="btn btn-ghost btn-xs text-primary-600 dark:text-primary-400"
                :disabled="
                  errorHandlingRuleForm.rules.length >=
                  ERROR_HANDLING_RULE_MAX_RULES
                "
                :title="t('admin.settings.errorHandlingRule.addRule')"
                :aria-label="t('admin.settings.errorHandlingRule.addRule')"
                @click="addErrorHandlingRule"
              >
                <Icon name="plus" size="xs" class="mr-1" />
                {{ t("admin.settings.errorHandlingRule.addRule") }}
              </button>
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
      </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";

import AppLayout from "@/components/layout/AppLayout.vue";
import Icon from "@/components/icons/Icon.vue";
import Toggle from "@/components/common/Toggle.vue";
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
  form_key: string;
  id: string;
  name: string;
  /** 原始输入文本，保存时才解析成 status_codes，避免输入过程中被重排 */
  status_codes_text: string;
  /** 原始输入文本，保存时才解析成 keywords */
  keywords_text: string;
  action: ErrorHandlingRuleAction;
  retry_count: number | null;
  exhausted_action: ErrorHandlingRuleExhaustedAction;
};

let errorHandlingRuleFormKeySequence = 0;

function nextErrorHandlingRuleFormKey() {
  errorHandlingRuleFormKeySequence += 1;
  return `error-handling-rule-${errorHandlingRuleFormKeySequence}`;
}

const errorHandlingRuleLoading = ref(true);
const errorHandlingRuleSaving = ref(false);
const errorHandlingRuleForm = reactive({
  enabled: false,
  default_retry_count: 1,
  rules: [] as ErrorHandlingRuleFormItem[],
});

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

function setRuleRetryCount(rule: ErrorHandlingRuleFormItem, raw: string) {
  const trimmed = raw.trim();
  if (trimmed === "") {
    rule.retry_count = null;
    return;
  }
  const parsed = parseInt(trimmed, 10);
  rule.retry_count = Number.isNaN(parsed)
    ? null
    : clampErrorHandlingRetryCount(parsed);
}

function addErrorHandlingRule() {
  if (errorHandlingRuleForm.rules.length >= ERROR_HANDLING_RULE_MAX_RULES) {
    return;
  }
  errorHandlingRuleForm.rules.push({
    form_key: nextErrorHandlingRuleFormKey(),
    id: "",
    name: "",
    status_codes_text: "",
    keywords_text: "",
    action: "retry",
    retry_count: null,
    exhausted_action: "default",
  });
}

function moveErrorHandlingRule(index: number, delta: number) {
  const target = index + delta;
  if (target < 0 || target >= errorHandlingRuleForm.rules.length) return;
  const rules = errorHandlingRuleForm.rules;
  [rules[index], rules[target]] = [rules[target], rules[index]];
}

function toErrorHandlingRuleFormItems(
  rules: ErrorHandlingRule[] | undefined,
): ErrorHandlingRuleFormItem[] {
  return (rules || []).map((rule) => ({
    form_key: nextErrorHandlingRuleFormKey(),
    id: rule.id,
    name: rule.name,
    status_codes_text: (rule.status_codes || []).join(", "),
    keywords_text: (rule.keywords || []).join("\n"),
    action: rule.action || "retry",
    retry_count: rule.retry_count ?? null,
    exhausted_action: rule.exhausted_action || "default",
  }));
}

/**
 * 解析并校验表单，返回可直接提交的 payload；校验失败时提示并返回 null。
 */
function buildErrorHandlingRulePayload(): ErrorHandlingRuleSettings | null {
  const rules: ErrorHandlingRule[] = [];
  for (const [index, rule] of errorHandlingRuleForm.rules.entries()) {
    const position = index + 1;
    const { codes, invalid } = parseStatusCodes(rule.status_codes_text);
    if (invalid.length > 0) {
      appStore.showError(
        t("admin.settings.errorHandlingRule.invalidStatusCode", {
          index: position,
          value: invalid.join(", "),
        }),
      );
      return null;
    }
    const keywords = parseKeywordList(rule.keywords_text);
    if (codes.length === 0 && keywords.length === 0) {
      appStore.showError(
        t("admin.settings.errorHandlingRule.emptyMatcher", {
          index: position,
        }),
      );
      return null;
    }
    rules.push({
      id: rule.id,
      name: rule.name,
      status_codes: codes,
      keywords,
      action: rule.action,
      retry_count:
        rule.retry_count === null
          ? null
          : clampErrorHandlingRetryCount(rule.retry_count),
      exhausted_action:
        rule.action === "passthrough" ? "default" : rule.exhausted_action,
    });
  }

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
  if (!payload) return;

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
