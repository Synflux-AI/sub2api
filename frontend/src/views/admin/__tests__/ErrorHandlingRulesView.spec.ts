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

function mountView() {
  return mount(ErrorHandlingRulesView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        Toggle: ToggleStub,
        Icon: true,
      },
    },
  });
}

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

  it("loads, reorders, adds, deletes, and saves error handling rules", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 2,
      rules: [
        {
          id: "rule-a",
          name: "Rule A",
          status_codes: [400],
          keywords: ["first"],
          action: "retry",
          retry_count: null,
        },
        {
          id: "rule-b",
          name: "Rule B",
          status_codes: [422],
          keywords: ["second"],
          action: "failover",
          retry_count: null,
        },
      ],
    });
    updateErrorHandlingRuleSettings.mockImplementation(async (payload) => ({
      ...payload,
      rules: payload.rules.map((rule: { id: string }, index: number) => ({
        ...rule,
        id: rule.id || `new-rule-${index}`,
      })),
    }));

    const wrapper = mountView();
    await flushPromises();

    const card = wrapper.get('[data-testid="error-handling-rule-card"]');
    expect(card.find('icon-stub[name="arrowUp"]').exists()).toBe(true);
    expect(card.find('icon-stub[name="arrowDown"]').exists()).toBe(true);
    expect(card.find('icon-stub[name="trash"]').exists()).toBe(true);

    const moveUpButtons = card.findAll(
      'button[aria-label="admin.settings.errorHandlingRule.moveUp"]',
    );
    expect(moveUpButtons).toHaveLength(2);
    await moveUpButtons[1].trigger("click");

    const addButton = card.get('[data-testid="error-handling-rule-add"]');
    expect(addButton.find('icon-stub[name="plus"]').exists()).toBe(true);
    await addButton.trigger("click");
    await addButton.trigger("click");
    expect(
      card.findAll(
        'input[placeholder="admin.settings.errorHandlingRule.namePlaceholder"]',
      ),
    ).toHaveLength(4);

    const deleteButtons = card.findAll('button[aria-label="common.delete"]');
    await deleteButtons[deleteButtons.length - 1].trigger("click");

    // 新增的规则必须至少填一个匹配条件，否则会被前端校验拦下
    const statusCodeInputs = card.findAll(
      '[data-testid="error-handling-rule-status-codes"]',
    );
    expect(statusCodeInputs).toHaveLength(3);
    await statusCodeInputs[2].setValue("500");

    await card.get('[data-testid="error-handling-rule-save"]').trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings).toHaveBeenCalledWith({
      enabled: true,
      default_retry_count: 2,
      rules: [
        expect.objectContaining({ id: "rule-b", name: "Rule B" }),
        expect.objectContaining({ id: "rule-a", name: "Rule A" }),
        expect.objectContaining({ id: "", name: "", status_codes: [500] }),
      ],
    });
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

  it("commits status code and keyword edits without a change event", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [
        {
          id: "rule-a",
          name: "Rule A",
          status_codes: [400],
          keywords: ["first"],
          action: "retry",
          retry_count: null,
        },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    const card = wrapper.get('[data-testid="error-handling-rule-card"]');
    await card
      .get('[data-testid="error-handling-rule-status-codes"]')
      .setValue("429, 529");
    await card
      .get('[data-testid="error-handling-rule-keywords"]')
      .setValue("overloaded\n  rate limit  \n\n");
    await card.get('[data-testid="error-handling-rule-save"]').trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings).toHaveBeenCalledWith({
      enabled: true,
      default_retry_count: 1,
      rules: [
        {
          id: "rule-a",
          name: "Rule A",
          status_codes: [429, 529],
          keywords: ["overloaded", "rate limit"],
          action: "retry",
          retry_count: null,
        },
      ],
    });
  });

  it("blocks saving invalid status codes with a localized error", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [
        {
          id: "rule-a",
          name: "Rule A",
          status_codes: [400],
          keywords: [],
          action: "retry",
          retry_count: null,
        },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    const card = wrapper.get('[data-testid="error-handling-rule-card"]');
    const statusCodes = card.get(
      '[data-testid="error-handling-rule-status-codes"]',
    );

    await statusCodes.setValue("4e2");
    await card.get('[data-testid="error-handling-rule-save"]').trigger("click");
    await flushPromises();
    expect(updateErrorHandlingRuleSettings).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith(
      "admin.settings.errorHandlingRule.invalidStatusCode",
    );

    showError.mockClear();
    await statusCodes.setValue("700");
    await card.get('[data-testid="error-handling-rule-save"]').trigger("click");
    await flushPromises();
    expect(updateErrorHandlingRuleSettings).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith(
      "admin.settings.errorHandlingRule.invalidStatusCode",
    );
  });

  it("blocks saving a rule without status codes or keywords", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [
        {
          id: "rule-a",
          name: "Rule A",
          status_codes: [400],
          keywords: ["first"],
          action: "retry",
          retry_count: null,
        },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    const card = wrapper.get('[data-testid="error-handling-rule-card"]');
    await card
      .get('[data-testid="error-handling-rule-status-codes"]')
      .setValue("");
    await card
      .get('[data-testid="error-handling-rule-keywords"]')
      .setValue("  \n ");
    await card.get('[data-testid="error-handling-rule-save"]').trigger("click");
    await flushPromises();

    expect(updateErrorHandlingRuleSettings).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith(
      "admin.settings.errorHandlingRule.emptyMatcher",
    );
  });

  it("always sends a clamped integer default retry count", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [],
    });

    const wrapper = mountView();
    await flushPromises();

    const card = wrapper.get('[data-testid="error-handling-rule-card"]');
    const defaultRetry = card.get(
      '[data-testid="error-handling-rule-default-retry"]',
    );

    await defaultRetry.setValue("");
    await card.get('[data-testid="error-handling-rule-save"]').trigger("click");
    await flushPromises();
    expect(updateErrorHandlingRuleSettings.mock.calls[0][0]).toMatchObject({
      default_retry_count: 0,
    });

    await defaultRetry.setValue("9");
    await card.get('[data-testid="error-handling-rule-save"]').trigger("click");
    await flushPromises();
    expect(updateErrorHandlingRuleSettings.mock.calls[1][0]).toMatchObject({
      default_retry_count: 4,
    });
  });

  it("warns about raw upstream body when the passthrough action is selected", async () => {
    getErrorHandlingRuleSettings.mockResolvedValue({
      enabled: true,
      default_retry_count: 1,
      rules: [
        {
          id: "rule-a",
          name: "Rule A",
          status_codes: [400],
          keywords: [],
          action: "retry",
          retry_count: null,
        },
      ],
    });

    const wrapper = mountView();
    await flushPromises();

    const card = wrapper.get('[data-testid="error-handling-rule-card"]');
    expect(card.text()).not.toContain(
      "admin.settings.errorHandlingRule.passthroughWarning",
    );

    await card
      .get('[data-testid="error-handling-rule-action"]')
      .setValue("passthrough");

    expect(card.text()).toContain(
      "admin.settings.errorHandlingRule.passthroughWarning",
    );
  });
});
