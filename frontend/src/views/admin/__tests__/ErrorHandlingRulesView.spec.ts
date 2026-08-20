import { beforeEach, describe, expect, it, vi } from "vitest";
import { defineComponent, h } from "vue";
import { flushPromises, mount } from "@vue/test-utils";

import ErrorHandlingRulesView from "../ErrorHandlingRulesView.vue";

const {
  getErrorHandlingRuleSettings,
  updateErrorHandlingRuleSettings,
  showError,
  showSuccess,
} = vi.hoisted(() => ({
  getErrorHandlingRuleSettings: vi.fn(),
  updateErrorHandlingRuleSettings: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}));

vi.mock("@/api", () => ({
  adminAPI: {
    settings: {
      getErrorHandlingRuleSettings,
      updateErrorHandlingRuleSettings,
    },
  },
}));

vi.mock("@/stores", () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showWarning: vi.fn(),
    showInfo: vi.fn(),
  }),
}));

vi.mock("@/utils/apiError", () => ({
  extractApiErrorMessage: () => "error",
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string>) =>
        key.replace(/\{(\w+)\}/g, (_, token) => params?.[token] ?? `{${token}}`),
      locale: { value: "zh-CN" },
    }),
  };
});

const AppLayoutStub = { template: "<div><slot /></div>" };

const ToggleStub = defineComponent({
  name: "Toggle",
  props: {
    modelValue: {
      type: Boolean,
      default: false,
    },
  },
  emits: ["update:modelValue"],
  inheritAttrs: false,
  setup(props, { attrs, emit }) {
    return () =>
      h("input", {
        ...attrs,
        class: "toggle-stub",
        type: "checkbox",
        checked: props.modelValue,
        onChange: (event: Event) => {
          emit("update:modelValue", (event.target as HTMLInputElement).checked);
        },
      });
  },
});

// BaseDialog teleports to body; render inline so the spec can drive it from the wrapper.
const BaseDialogStub = defineComponent({
  name: "BaseDialog",
  props: { show: { type: Boolean, default: false } },
  template:
    '<div v-if="show" data-testid="rule-dialog"><slot /><slot name="footer" /></div>',
});

// HelpTooltip teleports its bubble to body; render it inline so the spec can read
// the hover content straight off the row.
const HelpTooltipStub = defineComponent({
  name: "HelpTooltip",
  template:
    '<span data-testid="keywords-tooltip"><slot name="trigger" />' +
    '<span data-testid="keywords-tooltip-content"><slot /></span></span>',
});

const ConfirmDialogStub = defineComponent({
  name: "ConfirmDialog",
  props: { show: { type: Boolean, default: false } },
  emits: ["confirm", "cancel"],
  template:
    '<div v-if="show" data-testid="delete-dialog">' +
    '<button data-testid="delete-confirm" @click="$emit(\'confirm\')"></button>' +
    '<button data-testid="delete-cancel" @click="$emit(\'cancel\')"></button>' +
    "</div>",
});

function mountView() {
  return mount(ErrorHandlingRulesView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        Toggle: ToggleStub,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: ConfirmDialogStub,
        HelpTooltip: HelpTooltipStub,
        EmptyState: true,
        Icon: true,
      },
    },
  });
}

type Wrapper = ReturnType<typeof mountView>;

/** DataTable 的空状态也是一个 tr，靠 data-row-id 只挑出真正的数据行 */
function ruleRows(wrapper: Wrapper) {
  return wrapper.findAll("tbody tr[data-row-id]");
}

/** 关键字 badge 用 badge-gray，状态码用 badge-danger，靠这个把两者分开断言 */
function keywordBadges(row: ReturnType<typeof ruleRows>[number]) {
  return row.findAll(".badge-gray").map((badge) => badge.text());
}

async function openEditDialog(wrapper: Wrapper, rowIndex: number) {
  await ruleRows(wrapper)[rowIndex]
    .get('button[aria-label="common.edit"]')
    .trigger("click");
}

function dialog(wrapper: Wrapper) {
  return wrapper.get('[data-testid="rule-dialog"]');
}

/**
 * 确定按钮在 footer 插槽里，靠 form="error-handling-rule-form" 提交（与
 * RoutingStrategiesPanel 一致）。jsdom 不实现这条隐式提交，所以直接触发 submit。
 */
async function submitDialog(wrapper: Wrapper) {
  await dialog(wrapper).get("form").trigger("submit");
}

const RULE_A = {
  id: "rule-a",
  name: "Rule A",
  status_codes: [400],
  keywords: ["first"],
  action: "retry",
  retry_count: null,
  exhausted_action: "default",
};

const RULE_B = {
  id: "rule-b",
  name: "Rule B",
  status_codes: [422],
  keywords: ["second"],
  action: "failover",
  retry_count: null,
};

describe("admin ErrorHandlingRulesView", () => {
  beforeEach(() => {
    getErrorHandlingRuleSettings.mockReset();
    updateErrorHandlingRuleSettings.mockReset();
    showError.mockReset();
    showSuccess.mockReset();

    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: false,
      default_retry_count: 1,
      rules: [],
    });
    updateErrorHandlingRuleSettings.mockImplementation(
      async (payload) => payload,
    );
  });

  it("renders one table row per rule with its matcher summary", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 2,
      rules: [
        { ...RULE_A, keywords: ["k1", "k2", "k3"] },
        { ...RULE_B, keywords: [] },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    const rows = ruleRows(wrapper);
    expect(rows).toHaveLength(2);
    expect(rows[0].text()).toContain("Rule A");
    expect(rows[0].text()).toContain("400");
    // 只有前两条关键字渲染成 badge，其余折叠成计数（完整清单在悬浮气泡里）
    expect(keywordBadges(rows[0])).toEqual(['"k1"', '"k2"']);
    expect(rows[0].text()).toContain(
      "admin.settings.errorHandlingRule.keywordsMore",
    );
    // retry_count 为 null 时显示默认值
    expect(rows[0].text()).toContain(
      "admin.settings.errorHandlingRule.retryCountDefault",
    );
    expect(rows[1].text()).toContain(
      "admin.settings.errorHandlingRule.anyKeyword",
    );
  });

  // 折叠掉的关键字在表格里本来完全看不到，只能进弹窗才知道匹配的是什么
  it("lists every keyword in the hover tooltip when some are collapsed", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 2,
      rules: [{ ...RULE_A, keywords: ["k1", "k2", "k3", "k4"] }],
    });

    const wrapper = mountView();
    await flushPromises();

    const content = ruleRows(wrapper)[0].get(
      '[data-testid="keywords-tooltip-content"]',
    );
    // 气泡给的是完整清单，含已经渲染成 badge 的前两条
    for (const keyword of ["k1", "k2", "k3", "k4"]) {
      expect(content.text()).toContain(keyword);
    }
  });

  // 没有东西被折叠时挂个只显示已有内容的气泡纯属噪音
  it("omits the keyword tooltip when no keyword is collapsed", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 2,
      rules: [
        { ...RULE_A, keywords: ["k1", "k2"] },
        { ...RULE_B, keywords: [] },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    expect(wrapper.find('[data-testid="keywords-tooltip"]').exists()).toBe(
      false,
    );
  });

  // 行序即匹配优先级，可排序会让视图顺序和 rules 数组脱钩
  it("keeps every column unsortable so row order stays the match priority", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [RULE_A, RULE_B],
    });

    const wrapper = mountView();
    await flushPromises();

    expect(wrapper.findAll("thead th[aria-sort]")).toHaveLength(0);
  });

  // 总开关关掉时恰恰更需要确认配置还在
  it("keeps the rule table visible when the master switch is off", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: false,
      default_retry_count: 1,
      rules: [RULE_A],
    });

    const wrapper = mountView();
    await flushPromises();

    expect(ruleRows(wrapper)).toHaveLength(1);
  });

  it("reorders rules with the row move buttons and submits the new order", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 2,
      rules: [RULE_A, RULE_B],
    });

    const wrapper = mountView();
    await flushPromises();

    const rows = ruleRows(wrapper);
    // 首行 ↑ / 末行 ↓ 必须禁用
    expect(
      rows[0]
        .get('button[aria-label="admin.settings.errorHandlingRule.moveUp"]')
        .attributes("disabled"),
    ).toBeDefined();
    expect(
      rows[1]
        .get('button[aria-label="admin.settings.errorHandlingRule.moveDown"]')
        .attributes("disabled"),
    ).toBeDefined();

    await rows[1]
      .get('button[aria-label="admin.settings.errorHandlingRule.moveUp"]')
      .trigger("click");
    expect(ruleRows(wrapper)[0].text()).toContain("Rule B");

    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings).toHaveBeenCalledWith({
      enabled: true,
      default_retry_count: 2,
      rules: [
        expect.objectContaining({ id: "rule-b" }),
        expect.objectContaining({ id: "rule-a" }),
      ],
    });
  });

  it("round-trips the per-rule enabled toggle through save", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      // enabled 缺失代表存量规则，必须显示为已启用
      rules: [RULE_A, { ...RULE_B, enabled: false }],
    });

    const wrapper = mountView();
    await flushPromises();

    const toggles = wrapper.findAll("tbody tr .toggle-stub");
    expect(
      (toggles[0].element as HTMLInputElement).checked,
      "存量规则（无 enabled 字段）必须按启用渲染",
    ).toBe(true);
    expect((toggles[1].element as HTMLInputElement).checked).toBe(false);

    await toggles[0].setValue(false);
    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings.mock.calls[0][0].rules).toEqual([
      expect.objectContaining({ id: "rule-a", enabled: false }),
      expect.objectContaining({ id: "rule-b", enabled: false }),
    ]);
  });

  it("creates a rule through the dialog with a stable generated id", async () => {
    const wrapper = mountView();
    await flushPromises();

    expect(wrapper.find('[data-testid="rule-dialog"]').exists()).toBe(false);
    await wrapper.get('[data-testid="error-handling-rule-add"]').trigger("click");

    const form = dialog(wrapper);
    await form
      .get('[data-testid="error-handling-rule-dialog-name"]')
      .setValue("New rule");
    await form
      .get('[data-testid="error-handling-rule-status-codes"]')
      .setValue("500, 502");
    await form
      .get('[data-testid="error-handling-rule-keywords"]')
      .setValue("overloaded\n  rate limit  \n\n");
    await submitDialog(wrapper);

    expect(wrapper.find('[data-testid="rule-dialog"]').exists()).toBe(false);
    expect(ruleRows(wrapper)).toHaveLength(1);

    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    const [rule] = updateErrorHandlingRuleSettings.mock.calls[0][0].rules;
    expect(rule).toMatchObject({
      name: "New rule",
      enabled: true,
      status_codes: [500, 502],
      keywords: ["overloaded", "rate limit"],
      action: "retry",
      retry_count: null,
      exhausted_action: "default",
    });
    // 空 ID 会让后端按下标补 positional ID，排序一变重试计数就串规则
    expect(rule.id).toBeTruthy();
  });

  it("edits an existing rule through the dialog", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [RULE_A],
    });

    const wrapper = mountView();
    await flushPromises();
    await openEditDialog(wrapper, 0);

    const form = dialog(wrapper);
    expect(
      (
        form.get('[data-testid="error-handling-rule-status-codes"]')
          .element as HTMLInputElement
      ).value,
    ).toBe("400");

    await form
      .get('[data-testid="error-handling-rule-status-codes"]')
      .setValue("429, 529");
    await form
      .get('[data-testid="error-handling-rule-exhausted-action"]')
      .setValue("passthrough");
    await submitDialog(wrapper);

    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings.mock.calls[0][0].rules[0]).toEqual({
      id: "rule-a",
      name: "Rule A",
      enabled: true,
      status_codes: [429, 529],
      keywords: ["first"],
      action: "retry",
      retry_count: null,
      exhausted_action: "passthrough",
    });
  });

  it("keeps the dialog open and reports invalid status codes inline", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="error-handling-rule-add"]').trigger("click");

    const statusCodes = dialog(wrapper).get(
      '[data-testid="error-handling-rule-status-codes"]',
    );
    for (const invalid of ["4e2", "700"]) {
      await statusCodes.setValue(invalid);
      await submitDialog(wrapper);

      expect(wrapper.find('[data-testid="rule-dialog"]').exists()).toBe(true);
      expect(
        wrapper.get('[data-testid="error-handling-rule-dialog-error"]').text(),
      ).toBe("admin.settings.errorHandlingRule.invalidStatusCode");
      expect(ruleRows(wrapper)).toHaveLength(0);
    }
  });

  it("keeps the dialog open when neither status codes nor keywords are set", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="error-handling-rule-add"]').trigger("click");

    await dialog(wrapper)
      .get('[data-testid="error-handling-rule-keywords"]')
      .setValue("  \n ");
    await submitDialog(wrapper);

    expect(wrapper.find('[data-testid="rule-dialog"]').exists()).toBe(true);
    expect(
      wrapper.get('[data-testid="error-handling-rule-dialog-error"]').text(),
    ).toBe("admin.settings.errorHandlingRule.emptyMatcher");
    expect(ruleRows(wrapper)).toHaveLength(0);
  });

  it("warns about the raw upstream body when the passthrough action is selected", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [RULE_A],
    });

    const wrapper = mountView();
    await flushPromises();
    await openEditDialog(wrapper, 0);

    expect(dialog(wrapper).text()).not.toContain(
      "admin.settings.errorHandlingRule.passthroughWarning",
    );
    await dialog(wrapper)
      .get('[data-testid="error-handling-rule-action"]')
      .setValue("passthrough");
    expect(dialog(wrapper).text()).toContain(
      "admin.settings.errorHandlingRule.passthroughWarning",
    );
    // passthrough 不做同账号重试，也没有「全部失败后」可配
    expect(
      dialog(wrapper).find('[data-testid="error-handling-rule-retry-count"]')
        .exists(),
    ).toBe(false);
    expect(
      dialog(wrapper).find(
        '[data-testid="error-handling-rule-exhausted-action"]',
      ).exists(),
    ).toBe(false);
  });

  it("warns when the exhausted action returns the matched error", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [RULE_A],
    });

    const wrapper = mountView();
    await flushPromises();
    await openEditDialog(wrapper, 0);

    const exhausted = dialog(wrapper).get(
      '[data-testid="error-handling-rule-exhausted-action"]',
    );
    expect((exhausted.element as HTMLSelectElement).value).toBe("default");
    await exhausted.setValue("passthrough");
    expect(dialog(wrapper).text()).toContain(
      "admin.settings.errorHandlingRule.exhaustedActionWarning",
    );
  });

  it("deletes a rule only after the confirmation dialog is accepted", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [RULE_A, RULE_B],
    });

    const wrapper = mountView();
    await flushPromises();

    await ruleRows(wrapper)[0]
      .get('button[aria-label="common.delete"]')
      .trigger("click");
    await wrapper.get('[data-testid="delete-cancel"]').trigger("click");
    expect(ruleRows(wrapper)).toHaveLength(2);

    await ruleRows(wrapper)[0]
      .get('button[aria-label="common.delete"]')
      .trigger("click");
    await wrapper.get('[data-testid="delete-confirm"]').trigger("click");
    expect(ruleRows(wrapper)).toHaveLength(1);
    expect(ruleRows(wrapper)[0].text()).toContain("Rule B");
  });

  it("always sends a clamped integer default retry count", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [],
    });

    const wrapper = mountView();
    await flushPromises();

    const defaultRetry = wrapper.get(
      '[data-testid="error-handling-rule-default-retry"]',
    );

    await defaultRetry.setValue("");
    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();
    expect(updateErrorHandlingRuleSettings.mock.calls[0][0]).toMatchObject({
      default_retry_count: 0,
    });

    await defaultRetry.setValue("9");
    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();
    expect(updateErrorHandlingRuleSettings.mock.calls[1][0]).toMatchObject({
      default_retry_count: 4,
    });
  });

  it("never leaks form-only fields into the payload", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [RULE_A],
    });

    const wrapper = mountView();
    await flushPromises();
    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    const payload = updateErrorHandlingRuleSettings.mock.calls[0][0];
    expect(
      payload.rules.every(
        (rule: Record<string, unknown>) =>
          !("form_key" in rule) &&
          !("status_codes_text" in rule) &&
          !("keywords_text" in rule),
      ),
    ).toBe(true);
  });
});
