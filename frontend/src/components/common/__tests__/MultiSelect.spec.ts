import { mount } from '@vue/test-utils'
import { afterEach, describe, expect, it } from 'vitest'
import { nextTick } from 'vue'

import MultiSelect, { type MultiSelectOption } from '../MultiSelect.vue'

const options: MultiSelectOption[] = [
  { value: 1, label: 'Group Alpha' },
  { value: 2, label: 'Group Beta' },
  { value: 3, label: 'Group Gamma' },
]

let unmountWrapper: (() => void) | undefined

afterEach(() => {
  unmountWrapper?.()
  unmountWrapper = undefined
  document.body.innerHTML = ''
})

function mountMultiSelect(props: Record<string, unknown> = {}) {
  const wrapper = mount(MultiSelect, {
    props: {
      modelValue: [],
      options,
      exclusiveEmptyLabel: 'Global (all groups)',
      ...props,
    },
  })
  unmountWrapper = () => wrapper.unmount()
  return wrapper
}

async function openDropdown(wrapper: ReturnType<typeof mount>) {
  await wrapper.get('button.select-trigger').trigger('click')
  await nextTick()
  const dropdown = document.body.querySelector<HTMLElement>('.select-dropdown-portal')
  expect(dropdown).not.toBeNull()
  return dropdown as HTMLElement
}

function optionButtons(dropdown: HTMLElement) {
  // First rendered option button is always the exclusive item; the rest mirror `options`.
  return [...dropdown.querySelectorAll<HTMLButtonElement>('.select-options > button.select-option')]
}

describe('MultiSelect trigger display', () => {
  it('shows exclusiveEmptyLabel when the selection is empty', () => {
    const wrapper = mountMultiSelect({ modelValue: [] })
    const trigger = wrapper.get('button.select-trigger')
    expect(trigger.text()).toContain('Global (all groups)')
    expect(trigger.attributes('title')).toBe('Global (all groups)')
  })

  it('shows selected option labels joined together, with a title listing the full selection', () => {
    const wrapper = mountMultiSelect({ modelValue: [1, 2] })
    const trigger = wrapper.get('button.select-trigger')
    expect(trigger.text()).toContain('Group Alpha')
    expect(trigger.text()).toContain('Group Beta')
    expect(trigger.attributes('title')).toBe('Group Alpha、Group Beta')
  })

  it('appends a (N) count suffix once more than one option is selected', () => {
    const single = mountMultiSelect({ modelValue: [1] })
    expect(single.get('button.select-trigger').text()).not.toContain('(1)')

    const multiple = mountMultiSelect({ modelValue: [1, 2, 3] })
    expect(multiple.get('button.select-trigger').text()).toContain('(3)')
  })

  it('falls back to "#<value>" for a value that has no matching option (dangling id)', () => {
    const wrapper = mountMultiSelect({ modelValue: [99] })
    const trigger = wrapper.get('button.select-trigger')
    // Must never render as an empty string: that would look identical to the "global" state.
    expect(trigger.text()).toContain('#99')
    expect(trigger.text()).not.toContain('Global (all groups)')
    expect(trigger.attributes('title')).toBe('#99')
  })

  it('mixes resolved labels and "#<value>" fallbacks in modelValue order', () => {
    const wrapper = mountMultiSelect({ modelValue: [99, 1] })
    const trigger = wrapper.get('button.select-trigger')
    expect(trigger.attributes('title')).toBe('#99、Group Alpha')
    expect(trigger.text()).toContain('(2)')
  })
})

describe('MultiSelect accessibility labels', () => {
  it('uses the ariaLabel prop for the trigger and falls back to a generic literal', () => {
    const labelled = mountMultiSelect({ modelValue: [], ariaLabel: '分组' })
    expect(labelled.get('button.select-trigger').attributes('aria-label')).toBe('分组')

    const bare = mountMultiSelect({ modelValue: [] })
    expect(bare.get('button.select-trigger').attributes('aria-label')).toBe('Select options')
  })

  it('labels the search input with the searchPlaceholder prop', async () => {
    const wrapper = mountMultiSelect({ modelValue: [], searchable: true, searchPlaceholder: '搜索...' })
    const dropdown = await openDropdown(wrapper)

    const input = dropdown.querySelector<HTMLInputElement>('.select-search-input')!
    expect(input.getAttribute('placeholder')).toBe('搜索...')
    expect(input.getAttribute('aria-label')).toBe('搜索...')
  })

  it('defaults the search input placeholder/aria-label to a generic literal', async () => {
    const wrapper = mountMultiSelect({ modelValue: [], searchable: true })
    const dropdown = await openDropdown(wrapper)

    const input = dropdown.querySelector<HTMLInputElement>('.select-search-input')!
    expect(input.getAttribute('placeholder')).toBe('Search')
    expect(input.getAttribute('aria-label')).toBe('Search')
  })
})

describe('MultiSelect exclusive-item semantics', () => {
  it('renders the exclusive item as the first dropdown entry and marks it selected when modelValue is empty', async () => {
    const wrapper = mountMultiSelect({ modelValue: [] })
    const dropdown = await openDropdown(wrapper)
    const buttons = optionButtons(dropdown)

    expect(buttons[0].textContent).toContain('Global (all groups)')
    expect(buttons[0].getAttribute('aria-selected')).toBe('true')
  })

  it('checking the exclusive item emits an empty array', async () => {
    const wrapper = mountMultiSelect({ modelValue: [1, 2] })
    const dropdown = await openDropdown(wrapper)
    const [exclusiveButton] = optionButtons(dropdown)

    exclusiveButton.click()
    await nextTick()

    expect(wrapper.emitted('update:modelValue')).toEqual([[[]]])
  })

  it('checking any concrete option clears the exclusive selection', async () => {
    const wrapper = mountMultiSelect({ modelValue: [] })
    let dropdown = await openDropdown(wrapper)
    let buttons = optionButtons(dropdown)
    expect(buttons[0].getAttribute('aria-selected')).toBe('true') // exclusive selected initially

    const alphaButton = buttons.find((btn) => btn.textContent?.includes('Group Alpha'))!
    alphaButton.click()
    await nextTick()

    expect(wrapper.emitted('update:modelValue')).toEqual([[[1]]])

    // Simulate the parent applying the emitted v-model update.
    await wrapper.setProps({ modelValue: [1] })
    dropdown = document.body.querySelector<HTMLElement>('.select-dropdown-portal') as HTMLElement
    buttons = optionButtons(dropdown)
    expect(buttons[0].getAttribute('aria-selected')).toBe('false')
    const reselectedAlpha = buttons.find((btn) => btn.textContent?.includes('Group Alpha'))!
    expect(reselectedAlpha.getAttribute('aria-selected')).toBe('true')
  })

  it('toggles a concrete option off while leaving others selected', async () => {
    const wrapper = mountMultiSelect({ modelValue: [1, 2] })
    const dropdown = await openDropdown(wrapper)
    const buttons = optionButtons(dropdown)
    const alphaButton = buttons.find((btn) => btn.textContent?.includes('Group Alpha'))!

    alphaButton.click()
    await nextTick()

    expect(wrapper.emitted('update:modelValue')).toEqual([[[2]]])
  })
})

describe('MultiSelect search filtering', () => {
  it('filters options by label when searchable is enabled', async () => {
    const wrapper = mountMultiSelect({ modelValue: [], searchable: true })
    const dropdown = await openDropdown(wrapper)

    const input = dropdown.querySelector<HTMLInputElement>('.select-search-input')
    expect(input).not.toBeNull()

    input!.value = 'beta'
    input!.dispatchEvent(new Event('input'))
    await nextTick()

    const labels = optionButtons(dropdown)
      .slice(1) // drop the pinned exclusive item
      .map((btn) => btn.textContent?.trim())
    expect(labels).toEqual(['Group Beta'])
  })

  it('keeps the exclusive item visible regardless of the search query', async () => {
    const wrapper = mountMultiSelect({ modelValue: [], searchable: true })
    const dropdown = await openDropdown(wrapper)

    const input = dropdown.querySelector<HTMLInputElement>('.select-search-input')!
    input.value = 'nothing-matches'
    input.dispatchEvent(new Event('input'))
    await nextTick()

    const buttons = optionButtons(dropdown)
    expect(buttons).toHaveLength(1)
    expect(buttons[0].textContent).toContain('Global (all groups)')
    expect(dropdown.querySelector('.select-empty')).not.toBeNull()
  })

  it('does not render a search box when searchable is false', async () => {
    const wrapper = mountMultiSelect({ modelValue: [], searchable: false })
    const dropdown = await openDropdown(wrapper)
    expect(dropdown.querySelector('.select-search-input')).toBeNull()
  })

  it('defaults the empty-results placeholder to the literal "No results"', async () => {
    const wrapper = mountMultiSelect({ modelValue: [], searchable: true })
    const dropdown = await openDropdown(wrapper)

    const input = dropdown.querySelector<HTMLInputElement>('.select-search-input')!
    input.value = 'nothing-matches'
    input.dispatchEvent(new Event('input'))
    await nextTick()

    expect(dropdown.querySelector('.select-empty')?.textContent).toBe('No results')
  })

  it('uses the noResultsText prop for the empty-results placeholder when provided', async () => {
    const wrapper = mountMultiSelect({ modelValue: [], searchable: true, noResultsText: '无匹配选项' })
    const dropdown = await openDropdown(wrapper)

    const input = dropdown.querySelector<HTMLInputElement>('.select-search-input')!
    input.value = 'nothing-matches'
    input.dispatchEvent(new Event('input'))
    await nextTick()

    expect(dropdown.querySelector('.select-empty')?.textContent).toBe('无匹配选项')
  })

  it('defaults to auto: no search box for a short option list', async () => {
    const wrapper = mountMultiSelect({ modelValue: [] })
    const dropdown = await openDropdown(wrapper)
    expect(dropdown.querySelector('.select-search-input')).toBeNull()
  })
})

describe('MultiSelect dismissal', () => {
  it('closes the dropdown on Escape', async () => {
    const wrapper = mountMultiSelect({ modelValue: [] })
    await openDropdown(wrapper)

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    // The dropdown is wrapped in a <Transition>; give the leave hook a tick to run
    // before asserting removal (mirrors the isOpen flag rather than DOM presence).
    await nextTick()
    await nextTick()

    expect(wrapper.get('button.select-trigger').attributes('aria-expanded')).toBe('false')
  })

  it('closes the dropdown on an outside click', async () => {
    const wrapper = mountMultiSelect({ modelValue: [] })
    await openDropdown(wrapper)

    document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    await nextTick()
    await nextTick()

    expect(wrapper.get('button.select-trigger').attributes('aria-expanded')).toBe('false')
  })
})
