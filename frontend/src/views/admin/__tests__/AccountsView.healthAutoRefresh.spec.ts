import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import AccountsView from '../AccountsView.vue'

const {
  listAccounts,
  listWithEtag,
  getById,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  showError
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      getById,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  name: 'DataTableStub',
  props: { data: { type: Array, default: () => [] } },
  template: '<div />'
})

function mountView() {
  return mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: true,
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: true
      }
    }
  })
}

const UPDATED_AT = '2026-09-09T02:00:00Z'

const baseRow = {
  id: 42,
  name: 'health row',
  platform: 'anthropic',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  group_ids: [],
  extra: {},
  credentials: {},
  updated_at: UPDATED_AT,
  health_score: 100,
  health_tier: 0
}

function tableRows(wrapper: ReturnType<typeof mountView>) {
  return wrapper.findComponent(DataTableStub).props('data') as Array<Record<string, unknown>>
}

// 自动刷新走增量合并：拿到 200 也只替换「有变化」的行。健康分只存在 Redis，
// 变化时不会 bump accounts.updated_at，所以必须单独参与比对，否则「健康分」列
// 会一直停在页面加载时的值（#235）。
describe('admin AccountsView health score auto refresh', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({ items: [{ ...baseRow }], total: 1, page: 1, page_size: 20, pages: 1 })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'etag-0', data: null })
    getById.mockReset().mockResolvedValue({ ...baseRow, groups: [] })
    getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
    getAllProxies.mockReset().mockResolvedValue([])
    getAllGroups.mockReset().mockResolvedValue([])
    showError.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  async function mountWithAutoRefresh(nextRow: Record<string, unknown>) {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    listWithEtag.mockResolvedValue({
      notModified: false,
      etag: 'etag-1',
      data: { items: [nextRow], total: 1, page: 1, page_size: 20, pages: 1 }
    })

    const wrapper = mountView()
    await flushPromises()
    const before = tableRows(wrapper)[0]

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()

    return { wrapper, before, after: tableRows(wrapper)[0] }
  }

  it('adopts a new health score even though updated_at is unchanged', async () => {
    const { wrapper, after } = await mountWithAutoRefresh({ ...baseRow, health_score: 20, health_tier: 2 })

    expect(after.health_score).toBe(20)
    expect(after.health_tier).toBe(2)
    wrapper.unmount()
  })

  it('adopts a tier change that leaves the rounded score untouched', async () => {
    const { wrapper, after } = await mountWithAutoRefresh({ ...baseRow, health_tier: 1 })

    expect(after.health_tier).toBe(1)
    wrapper.unmount()
  })

  // ttft 目前不在本页渲染，而它的 updated_at 每轮巡检都变；纳入比对会让整表
  // 每次自动刷新全量重建，正是增量合并要避免的事。
  it('keeps the existing row object when only the ttft snapshot moved', async () => {
    const { wrapper, before, after } = await mountWithAutoRefresh({
      ...baseRow,
      ttft: { account_id: 42, p50_ms: 900, p95_ms: 2100, samples: 12, ratio: 1.1, worst_ratio: 1.3, degraded: false, updated_at: '2026-09-09T02:00:30Z' }
    })

    expect(after).toBe(before)
    wrapper.unmount()
  })
})
