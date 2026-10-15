import { defineComponent } from "vue";
import { flushPromises, mount } from "@vue/test-utils";
import { createPinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { AdminGroup } from "@/types";
import GroupsView from "@/views/admin/GroupsView.vue";

const {
  listGroups,
  getModelAllowlistCandidates,
  getUsageSummary,
  getCapacitySummary,
  getLiveCapability,
  updateGroup,
  showError,
} = vi.hoisted(() => ({
  listGroups: vi.fn(),
  getModelAllowlistCandidates: vi.fn(),
  getUsageSummary: vi.fn(),
  getCapacitySummary: vi.fn(),
  getLiveCapability: vi.fn(),
  updateGroup: vi.fn(),
  showError: vi.fn(),
}));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      getAll: vi.fn(),
      getModelAllowlistCandidates,
      getUsageSummary,
      getCapacitySummary,
      getLiveCapability,
      create: vi.fn(),
      update: updateGroup,
      delete: vi.fn(),
      duplicate: vi.fn(),
      updateSortOrder: vi.fn(),
    },
    accounts: {
      list: vi.fn(),
      getById: vi.fn(),
    },
  },
}));

vi.mock("@/stores/app", () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
  }),
}));

// 本仓库 GroupsView 依赖 authStore.isSimpleMode 控制简易模式下的列/操作显隐，需一并 mock。
vi.mock("@/stores/auth", () => ({
  useAuthStore: () => ({ isSimpleMode: false }),
}));

vi.mock("@/stores/onboarding", () => ({
  useOnboardingStore: () => ({
    isCurrentStep: vi.fn(() => false),
    nextStep: vi.fn(),
  }),
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

const sourceGroup = {
  id: 42,
  name: "OpenAI",
  description: null,
  platform: "openai",
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: "active",
  subscription_type: "standard",
  daily_limit_usd: null,
  weekly_limit_usd: null,
  monthly_limit_usd: null,
  long_context_pricing_enabled: true,
  force_openai_fast: false,
  free_openai_fast: false,
  model_pricing: [],
  profit_control_enabled: false,
  profit_min_margin: 0,
  profit_safety_buffer: 0,
  allow_image_generation: false,
  allow_batch_image_generation: false,
  image_rate_independent: false,
  image_rate_multiplier: 1,
  batch_image_discount_multiplier: 0.5,
  batch_image_hold_multiplier: 0.6,
  image_price_1k: null,
  image_price_2k: null,
  image_price_4k: null,
  video_rate_independent: false,
  video_rate_multiplier: 1,
  video_price_480p: null,
  video_price_720p: null,
  video_price_1080p: null,
  web_search_price_per_call: null,
  search_price_per_1k: null,
  audio_realtime_price_per_min: null,
  audio_tts_price_per_million_chars: null,
  audio_stt_price_per_hour: null,
  peak_rate_enabled: false,
  peak_start: "",
  peak_end: "",
  peak_rate_multiplier: 1,
  claude_code_only: false,
  fallback_group_id: null,
  fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false,
  allow_live: false,
  require_oauth_only: false,
  require_privacy_set: false,
  created_at: "2026-09-05T00:00:00Z",
  updated_at: "2026-09-05T00:00:00Z",
  model_routing: null,
  model_routing_enabled: false,
  mcp_xml_inject: true,
  supported_model_scopes: [],
  account_count: 1,
  active_account_count: 1,
  rate_limited_account_count: 0,
  models_list_config: undefined,
  codex_models_manifest_config: {
    enabled: false,
    account_ids: [],
    fallback_to_scheduler: false,
  },
  sort_order: 10,
  model_rate_multipliers: [{ models: ["gpt-5.1"], rate_multiplier: 0.6 }],
} satisfies AdminGroup;

const AppLayoutStub = defineComponent({
  template: "<main><slot /></main>",
});

const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>',
});

const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
  },
  template: '<div><div v-if="data.length"><slot name="cell-actions" :row="data[0]" /></div></div>',
});

const BaseDialogStub = defineComponent({
  props: {
    show: { type: Boolean, default: false },
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
});

// 表单提交前会调用这两个子组件暴露的 validate()；桩组件必须同样暴露。
const ValidatingStub = (name: string) =>
  defineComponent({
    name,
    setup(_, { expose }) {
      expose({ validate: () => true, resetValidation: () => undefined });
      return () => null;
    },
  });

const mountView = () =>
  mount(GroupsView, {
    global: {
      plugins: [createPinia()],
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        Pagination: true,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        PlatformIcon: true,
        Icon: true,
        GroupCapacityBadge: true,
        GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true,
        ReasoningEffortPolicyFields: ValidatingStub("ReasoningEffortPolicyFields"),
        CodexManifestAccountsField: ValidatingStub("CodexManifestAccountsField"),
        PricingEntryCard: true,
        VueDraggable: true,
      },
    },
  });

describe("GroupsView model rate multipliers", () => {
  beforeEach(() => {
    localStorage.clear();
    for (const fn of [listGroups, getModelAllowlistCandidates, getUsageSummary, getCapacitySummary, getLiveCapability, updateGroup, showError]) {
      fn.mockReset();
    }
    listGroups.mockResolvedValue({ items: [sourceGroup], total: 1, page: 1, page_size: 20, pages: 1 });
    getModelAllowlistCandidates.mockResolvedValue([]);
    getUsageSummary.mockResolvedValue([]);
    getCapacitySummary.mockResolvedValue([]);
    getLiveCapability.mockResolvedValue({ supported: false });
    updateGroup.mockResolvedValue(sourceGroup);
  });

  const openEdit = async () => {
    const wrapper = mountView();
    await flushPromises();
    const editButton = wrapper.findAll("button").find((button) => button.text().includes("common.edit"));
    await editButton!.trigger("click");
    await flushPromises();
    return wrapper;
  };

  it("loads existing rules into the edit form and submits edits", async () => {
    const wrapper = await openEdit();
    const editor = wrapper.get('[data-testid="group-model-rate-multipliers"]');
    const values = editor.findAll('[data-testid="group-model-rate-value"]');
    expect(values).toHaveLength(1);
    expect((values[0].element as HTMLInputElement).value).toBe("0.6");

    await values[0].setValue("0.55");
    await editor.get('[data-testid="group-model-rate-add"]').trigger("click");
    // 未填模型的新卡片提交时被丢弃
    expect(editor.findAll('[data-testid="group-model-rate-entry"]')).toHaveLength(2);

    await wrapper.get("#edit-group-form").trigger("submit");
    await flushPromises();
    expect(showError).not.toHaveBeenCalled();
    expect(updateGroup).toHaveBeenCalledTimes(1);
    expect(updateGroup.mock.calls[0][1].model_rate_multipliers).toEqual([
      { models: ["gpt-5.1"], rate_multiplier: 0.55 },
    ]);
    wrapper.unmount();
  });

  it("blocks submit on an invalid rate", async () => {
    const wrapper = await openEdit();
    await wrapper.get('[data-testid="group-model-rate-value"]').setValue("0");
    await wrapper.get("#edit-group-form").trigger("submit");
    await flushPromises();
    expect(showError).toHaveBeenCalledWith("admin.groups.modelRates.errorRate");
    expect(updateGroup).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it("removing every rule submits an empty list", async () => {
    const wrapper = await openEdit();
    await wrapper.get('[data-testid="group-model-rate-remove"]').trigger("click");
    await wrapper.get("#edit-group-form").trigger("submit");
    await flushPromises();
    expect(updateGroup.mock.calls[0][1].model_rate_multipliers).toEqual([]);
    wrapper.unmount();
  });
});
