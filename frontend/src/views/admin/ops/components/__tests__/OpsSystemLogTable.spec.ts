import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import OpsSystemLogTable from '../OpsSystemLogTable.vue'
import LogRetentionSelect from '../LogRetentionSelect.vue'
import enLocale from '@/i18n/locales/en'
import zhLocale from '@/i18n/locales/zh'

const mockListSystemLogs = vi.fn()
const mockCleanupSystemLogs = vi.fn()
const mockGetSystemLogSinkHealth = vi.fn()
const mockGetRuntimeLogConfig = vi.fn()
const mockUpdateRuntimeLogConfig = vi.fn()
const mockShowError = vi.fn()
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
    updateRuntimeLogConfig: (...args: any[]) => mockUpdateRuntimeLogConfig(...args),
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: (...args: any[]) => mockShowError(...args),
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
  persist_access_logs: false,
  enable_sampling: false,
  sampling_initial: 100,
  sampling_thereafter: 100,
  caller: true,
  stacktrace_level: 'error',
  retention_days: 30,
  request_retention_days: 90,
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
    mockUpdateRuntimeLogConfig.mockImplementation(async config => config)
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

  it('keeps database access-log persistence opt-in', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    const label = wrapper.findAll('label').find((item) => item.text().includes('admin.ops.systemLogs.persistAccessLogs'))
    expect(label).toBeDefined()
    expect((label!.find('input').element as HTMLInputElement).checked).toBe(false)
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


describe('rolling log retention settings', () => {
  const mountTable = () => mount(OpsSystemLogTable, {
    global: { stubs: { Select: SelectStub, Pagination: PaginationStub } }
  })
  const saveButton = (wrapper: ReturnType<typeof mountTable>) => wrapper.findAll('button').find(button => button.text() === 'admin.ops.systemLogs.saveAndApply')!

  beforeEach(() => {
    vi.clearAllMocks()
    mockListSystemLogs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20 })
    mockGetSystemLogSinkHealth.mockResolvedValue(sinkHealth)
    mockGetRuntimeLogConfig.mockResolvedValue(runtimeConfig)
    mockUpdateRuntimeLogConfig.mockImplementation(async config => config)
  })

  it('loads and saves separate operations and request retention windows', async () => {
    const wrapper = mountTable()
    await flushPromises()
    const fields = wrapper.findAllComponents(LogRetentionSelect)
    expect(fields.map(field => field.props('modelValue'))).toEqual([30, 90])
    fields[0]!.vm.$emit('update:modelValue', 7)
    fields[1]!.vm.$emit('update:modelValue', 180)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ retention_days: 7, request_retention_days: 180 }))
    wrapper.unmount()
  })

  it('supports forever and rejects fractional or empty custom days', async () => {
    const wrapper = mountTable()
    await flushPromises()
    const requestField = wrapper.findAllComponents(LogRetentionSelect)[1]!
    requestField.vm.$emit('update:modelValue', 0)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(mockUpdateRuntimeLogConfig).toHaveBeenCalledWith(expect.objectContaining({ request_retention_days: 0 }))
    mockUpdateRuntimeLogConfig.mockClear()
    for (const value of [1.5, Number.NaN, -1, 3651]) {
      requestField.vm.$emit('update:modelValue', value)
      await saveButton(wrapper).trigger('click')
    }
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    expect(mockShowError).toHaveBeenCalledWith('admin.ops.systemLogs.retentionDaysInvalid')
    wrapper.unmount()
  })

  it('does not overwrite saved retention with defaults after a load failure', async () => {
    mockGetRuntimeLogConfig.mockRejectedValue(new Error('unavailable'))
    const wrapper = mountTable()
    await flushPromises()
    expect(saveButton(wrapper).attributes('disabled')).toBeDefined()
    await saveButton(wrapper).trigger('click')
    expect(mockUpdateRuntimeLogConfig).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
