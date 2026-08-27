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

/**
 * 行内优先级输入刻意用 change 而不是 input 提交：打字过程中实时重排会让
 * "12" 在敲到 "1" 时就把行甩走。测试也必须走 change 才是真实交互。
 */
async function setRowPriority(wrapper: Wrapper, ruleId: string, value: string) {
  const input = wrapper.get(
    `[data-testid="error-handling-rule-priority-${ruleId}"]`,
  );
  (input.element as HTMLInputElement).value = value;
  await input.trigger("change");
  return input;
}

function rowPriorities(wrapper: Wrapper) {
  return ruleRows(wrapper).map(
    (row) =>
      (row.get('input[type="number"]').element as HTMLInputElement).value,
  );
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

  // 行序完全由 priority 决定，任何一列可排序都会让视图顺序和优先级脱钩
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

  // 行内优先级数字取代了 ↑↓ 按钮：改完数字失焦，行要立刻按新优先级重排，
  // 保存时提交的也必须是排序后的数组（后端对相同 priority 按数组顺序稳定排序）
  it("reorders rules when a row priority changes and submits the sorted array", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 2,
      rules: [
        { ...RULE_A, priority: 1 },
        { ...RULE_B, priority: 2 },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    // ↑↓ 按钮已删除，行内数字是唯一的调序入口
    expect(
      wrapper
        .find('button[aria-label="admin.settings.errorHandlingRule.moveUp"]')
        .exists(),
    ).toBe(false);
    expect(
      wrapper
        .find('button[aria-label="admin.settings.errorHandlingRule.moveDown"]')
        .exists(),
    ).toBe(false);

    await setRowPriority(wrapper, "rule-a", "5");
    expect(ruleRows(wrapper)[0].text()).toContain("Rule B");
    expect(rowPriorities(wrapper)).toEqual(["2", "5"]);

    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings).toHaveBeenCalledWith({
      enabled: true,
      default_retry_count: 2,
      rules: [
        expect.objectContaining({ id: "rule-b", priority: 2 }),
        expect.objectContaining({ id: "rule-a", priority: 5 }),
      ],
    });
  });

  // 后端承诺按 priority 升序返回，但前端不能靠这个承诺：视图顺序就是匹配顺序，
  // 一旦接口顺序和 priority 不一致，用户看到的优先级就是错的
  it("renders rules in ascending priority order even when the API returns them shuffled", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [
        { ...RULE_A, priority: 30 },
        { ...RULE_B, priority: 10 },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    const rows = ruleRows(wrapper);
    expect(rows[0].text()).toContain("Rule B");
    expect(rows[1].text()).toContain("Rule A");
    expect(rowPriorities(wrapper)).toEqual(["10", "30"]);
  });

  // 存量数据没有 priority 字段，缺字段时必须按下标补 1..N，不能全塞 0/NaN
  it("falls back to the row index when the API omits priority", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [RULE_A, RULE_B],
    });

    const wrapper = mountView();
    await flushPromises();

    expect(rowPriorities(wrapper)).toEqual(["1", "2"]);
  });

  // 非法输入不能进本地状态：0 / 1000 会被后端拒掉，空串会让整行优先级变 NaN
  it("clamps an inline priority into the 1-999 range", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [{ ...RULE_A, priority: 7 }],
    });

    const wrapper = mountView();
    await flushPromises();

    const zero = await setRowPriority(wrapper, "rule-a", "0");
    // 输入框自己也要回显收敛后的值，否则界面上还留着非法的 0
    expect((zero.element as HTMLInputElement).value).toBe("1");

    await setRowPriority(wrapper, "rule-a", "1000");
    expect(rowPriorities(wrapper)).toEqual(["999"]);

    // 空串没有可用数值，保留改动前的值而不是掉成 0
    await setRowPriority(wrapper, "rule-a", "");
    expect(rowPriorities(wrapper)).toEqual(["999"]);

    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings.mock.calls[0][0].rules[0]).toMatchObject(
      { id: "rule-a", priority: 999 },
    );
  });

  // 新增规则默认排到最末，否则每加一条都要手动改数字才不会插队
  it("defaults the dialog priority to max priority + 1", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [
        { ...RULE_A, priority: 3 },
        { ...RULE_B, priority: 8 },
      ],
    });

    const wrapper = mountView();
    await flushPromises();
    await wrapper
      .get('[data-testid="error-handling-rule-add"]')
      .trigger("click");

    expect(
      (
        dialog(wrapper).get('[data-testid="error-handling-rule-dialog-priority"]')
          .element as HTMLInputElement
      ).value,
    ).toBe("9");
  });

  // 弹窗里的优先级是自由文本，确定时才解析：非法值要就地报错而不是静默塞个兜底值
  it("keeps the dialog open and reports an out-of-range priority inline", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper
      .get('[data-testid="error-handling-rule-add"]')
      .trigger("click");

    const form = dialog(wrapper);
    await form
      .get('[data-testid="error-handling-rule-status-codes"]')
      .setValue("500");

    for (const invalid of ["0", "1000", "abc"]) {
      await form
        .get('[data-testid="error-handling-rule-dialog-priority"]')
        .setValue(invalid);
      await submitDialog(wrapper);

      expect(wrapper.find('[data-testid="rule-dialog"]').exists()).toBe(true);
      expect(
        wrapper.get('[data-testid="error-handling-rule-dialog-error"]').text(),
      ).toBe("admin.settings.errorHandlingRule.invalidPriority");
      expect(ruleRows(wrapper)).toHaveLength(0);
    }
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
    // 空列表时 max+1 退化成 1
    expect(
      (
        form.get('[data-testid="error-handling-rule-dialog-priority"]')
          .element as HTMLInputElement
      ).value,
    ).toBe("1");
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
      priority: 1,
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
      priority: 1,
      status_codes: [429, 529],
      keywords: ["first"],
      action: "retry",
      retry_count: null,
      exhausted_action: "passthrough",
      // #189：存量规则没有 platforms，回填成 anthropic 后必须显式发出去 ——
      // 省略这个 key 会被后端当成存量规则，绕一圈还是 anthropic，但语义就靠不住了。
      platforms: ["anthropic"],
      max_upstream_latency_ms: 0,
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

  // ==================== #189 适用平台 / 上游耗时上限 ====================

  it("treats a legacy rule without platforms as anthropic-only", async () => {
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
    expect(payload.rules[0].platforms).toEqual(["anthropic"]);
    expect(payload.rules[0].max_upstream_latency_ms).toBe(0);
  });

  it("defaults a newly created rule to anthropic", async () => {
    const wrapper = mountView();
    await flushPromises();

    await wrapper
      .get('[data-testid="error-handling-rule-add"]')
      .trigger("click");
    await dialog(wrapper)
      .get('[data-testid="error-handling-rule-status-codes"]')
      .setValue("500");
    await submitDialog(wrapper);
    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    const payload = updateErrorHandlingRuleSettings.mock.calls[0][0];
    expect(payload.rules[0].platforms).toEqual(["anthropic"]);
  });

  // 「全平台」是靠全部勾上表达的，所以一个都不勾必须在提交前拦住：空数组到了后端
  // 会被 normalize 当成存量配置，静默收窄成 anthropic —— 与管理员的意图相反。
  it("blocks the dialog when no platform is selected", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [{ ...RULE_A, platforms: ["anthropic"] }],
    });

    const wrapper = mountView();
    await flushPromises();
    await openEditDialog(wrapper, 0);

    await dialog(wrapper)
      .get('[data-testid="error-handling-rule-platform-anthropic"]')
      .setValue(false);
    await submitDialog(wrapper);

    expect(wrapper.find('[data-testid="rule-dialog"]').exists()).toBe(true);
    expect(
      wrapper
        .get('[data-testid="error-handling-rule-dialog-error"]')
        .text(),
    ).toContain("platformsRequired");
  });

  it("round-trips platforms and the upstream latency ceiling through the dialog", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [
        {
          ...RULE_B,
          platforms: ["openai"],
          max_upstream_latency_ms: 5000,
          exhausted_action: "default",
        },
      ],
    });

    const wrapper = mountView();
    await flushPromises();
    await openEditDialog(wrapper, 0);

    const openai = dialog(wrapper).get(
      '[data-testid="error-handling-rule-platform-openai"]',
    );
    expect((openai.element as HTMLInputElement).checked).toBe(true);
    const anthropic = dialog(wrapper).get(
      '[data-testid="error-handling-rule-platform-anthropic"]',
    );
    expect((anthropic.element as HTMLInputElement).checked).toBe(false);
    expect(
      (
        dialog(wrapper).get(
          '[data-testid="error-handling-rule-max-upstream-latency"]',
        ).element as HTMLInputElement
      ).value,
    ).toBe("5000");

    await anthropic.setValue(true);
    await dialog(wrapper)
      .get('[data-testid="error-handling-rule-max-upstream-latency"]')
      .setValue("8000");
    await submitDialog(wrapper);
    await wrapper
      .get('[data-testid="error-handling-rule-save"]')
      .trigger("click");
    await flushPromises();

    const payload = updateErrorHandlingRuleSettings.mock.calls[0][0];
    expect(payload.rules[0].platforms).toEqual(["anthropic", "openai"]);
    expect(payload.rules[0].max_upstream_latency_ms).toBe(8000);
  });

  it("rejects a negative upstream latency ceiling", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [{ ...RULE_A, platforms: ["anthropic"] }],
    });

    const wrapper = mountView();
    await flushPromises();
    await openEditDialog(wrapper, 0);

    await dialog(wrapper)
      .get('[data-testid="error-handling-rule-max-upstream-latency"]')
      .setValue("-1");
    await submitDialog(wrapper);

    expect(wrapper.find('[data-testid="rule-dialog"]').exists()).toBe(true);
    expect(
      wrapper.get('[data-testid="error-handling-rule-dialog-error"]').text(),
    ).toContain("invalidMaxUpstreamLatency");
  });
});
