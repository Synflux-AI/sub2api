import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import OpsSystemLogTable from '../OpsSystemLogTable.vue'
import enLocale from '@/i18n/locales/en'
import zhLocale from '@/i18n/locales/zh'

const mockListSystemLogs = vi.fn()
const mockCleanupSystemLogs = vi.fn()
const mockGetSystemLogSinkHealth = vi.fn()
const mockGetRuntimeLogConfig = vi.fn()
const mockCopyToClipboard = vi.fn()
const defaultMatchMedia = window.matchMedia

const mockMatchMedia = (matches: boolean) => ((query: string) => ({
  matches,
  media: query,
  onchange: null,
  addListener: vi.fn(),
  removeListener: vi.fn(),
  addEventListener: vi.fn(),
  removeEventListener: vi.fn(),
  dispatchEvent: vi.fn(),
})) as typeof window.matchMedia

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    listSystemLogs: (...args: any[]) => mockListSystemLogs(...args),
    cleanupSystemLogs: (...args: any[]) => mockCleanupSystemLogs(...args),
    getSystemLogSinkHealth: (...args: any[]) => mockGetSystemLogSinkHealth(...args),
    getRuntimeLogConfig: (...args: any[]) => mockGetRuntimeLogConfig(...args),
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
  }),
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard: (...args: any[]) => mockCopyToClipboard(...args),
  }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const SelectStub = defineComponent({
  name: 'SelectControlStub',
  props: {
    modelValue: {
      type: [String, Number],
      default: '',
    },
  },
  emits: ['update:modelValue'],
  template: '<div class="select-stub" />',
})

const PaginationStub = defineComponent({
  name: 'PaginationStub',
  template: '<div class="pagination-stub" />',
})

const runtimeConfig = {
  level: 'info',
  enable_sampling: false,
  sampling_initial: 100,
  sampling_thereafter: 100,
  caller: true,
  stacktrace_level: 'error',
  retention_days: 30,
}

const sinkHealth = {
  queue_depth: 0,
  queue_capacity: 5000,
  dropped_count: 0,
  write_failed_count: 0,
  written_count: 1,
  avg_write_delay_ms: 0,
}

describe('OpsSystemLogTable', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    window.matchMedia = defaultMatchMedia
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockCopyToClipboard.mockResolvedValue(true)
    mockListSystemLogs.mockResolvedValue({
      items: [
        {
          id: 1,
          created_at: '2026-07-14T00:10:01Z',
          host: 'api-node-1',
          level: 'warn',
          component: 'app',
          message: 'request failed',
          request_id: 'req-1',
          api_key_id: 7,
          extra: {
            trace_id: 'trace-exact-1',
            rule_id: 23,
            rule_name: 'Retry overloads',
            rule_action: 'retry',
            outcome: 'matched',
            upstream_status_code: 529,
            retry: { attempt: 2, reasons: ['timeout', true] },
            retry_exhausted: false,
          },
        },
      ],
      total: 1,
      page: 1,
      page_size: 20,
    })
    mockCleanupSystemLogs.mockResolvedValue({ deleted: 1 })
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
  })

  it('renders the host and sends it with list and cleanup filters', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('api-node-1')

    const hostLabel = wrapper.findAll('label').find((label) => label.text().includes('admin.ops.systemLogs.host'))
    expect(hostLabel).toBeDefined()
    await hostLabel!.find('input').setValue(' api-node-2 ')

    const searchButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.search')
    expect(searchButton).toBeDefined()
    await searchButton!.trigger('click')
    await flushPromises()

    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ host: 'api-node-2' }))

    const cleanupButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.cleanCurrentFilters')
    expect(cleanupButton).toBeDefined()
    await cleanupButton!.trigger('click')
    await flushPromises()

    expect(mockCleanupSystemLogs).toHaveBeenCalledWith(expect.objectContaining({ host: 'api-node-2' }))
  })

  it.each([
    ['zh', zhLocale],
    ['en', enLocale],
  ])('defines the Host translation for %s', (_name, locale) => {
    expect(locale.admin.ops.systemLogs.host).toBe('Host')
  })

  it('expands all base and extra fields, formats nested values, and collapses again', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('trace=trace-exact-1')
    expect(wrapper.find('[data-testid="system-log-details"]').exists()).toBe(false)

    const toggle = wrapper.get('[data-testid="toggle-system-log-1"]')
    await toggle.trigger('click')

    const details = wrapper.get('[data-testid="system-log-details"]')
    expect(details.text()).toContain('request_id')
    expect(details.text()).toContain('api_key_id')
    expect(details.text()).toContain('trace_id')
    expect(details.text()).toContain('rule_id')
    expect(details.text()).toContain('rule_action')
    expect(details.text()).toContain('upstream_status_code')
    expect(details.text()).toContain('"attempt": 2')
    expect(details.text()).toContain('"timeout"')
    expect(details.text()).toContain('false')
    expect(details.text()).not.toContain('[object Object]')

    await toggle.trigger('click')
    expect(wrapper.find('[data-testid="system-log-details"]').exists()).toBe(false)
  })

  it('copies the complete log JSON', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    await wrapper.get('[data-testid="toggle-system-log-1"]').trigger('click')
    await wrapper.get('[data-testid="copy-system-log-json"]').trigger('click')

    expect(mockCopyToClipboard).toHaveBeenCalledTimes(1)
    const [json, successMessage] = mockCopyToClipboard.mock.calls[0]
    expect(JSON.parse(json)).toMatchObject({
      id: 1,
      request_id: 'req-1',
      extra: {
        trace_id: 'trace-exact-1',
        rule_id: 23,
        retry: { attempt: 2, reasons: ['timeout', true] },
      },
    })
    expect(successMessage).toBe('admin.ops.systemLogs.copySuccess')
  })

  it('expands the same complete details in the mobile card view', async () => {
    window.matchMedia = mockMatchMedia(false)
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    expect(wrapper.find('table').exists()).toBe(false)
    await wrapper.get('[data-testid="toggle-system-log-1"]').trigger('click')
    expect(wrapper.get('[data-testid="system-log-details"]').text()).toContain('trace_id')
  })

  it('trims trace_id for list and cleanup, preserves the time window, and resets it', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    const traceLabel = wrapper.findAll('label').find((label) => label.text().includes('admin.ops.systemLogs.traceId'))
    expect(traceLabel).toBeDefined()
    await traceLabel!.find('input').setValue('  trace-exact-1  ')

    const searchButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.search')
    await searchButton!.trigger('click')
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ trace_id: 'trace-exact-1' }))

    const cleanupButton = wrapper.findAll('button').find((button) => button.text() === 'admin.ops.systemLogs.cleanCurrentFilters')
    await cleanupButton!.trigger('click')
    await flushPromises()
    expect(mockCleanupSystemLogs).toHaveBeenCalledWith(expect.objectContaining({
      trace_id: 'trace-exact-1',
      start_time: expect.any(String),
      end_time: expect.any(String),
    }))
    const cleanupPayload = mockCleanupSystemLogs.mock.calls[0][0]
    expect(new Date(cleanupPayload.end_time).getTime() - new Date(cleanupPayload.start_time).getTime()).toBe(60 * 60 * 1000)

    const resetButton = wrapper.findAll('button').find((button) => button.text() === 'common.reset')
    await resetButton!.trigger('click')
    await flushPromises()
    expect(traceLabel!.find('input').element.value).toBe('')
    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.not.objectContaining({ trace_id: expect.anything() }))
  })
})
