import { mount } from '@vue/test-utils'
import { afterEach, describe, expect, it } from 'vitest'
import { nextTick } from 'vue'

import MultiSelect, { type MultiSelectOption } from '../MultiSelect.vue'

/**
 * issue #171 给 MultiSelect 加了两个**可选**作用域插槽（selected / option），
 * 让 API Key 的分组多选能复用 GroupBadge / GroupOptionItem 的富渲染。
 *
 * 这个文件守的是两件事：
 *  1. 不传插槽时渲染与改动前逐字相同 —— #170 的路由策略面板就是这么用的，不能回归；
 *  2. 传插槽时能拿到完整的 option 对象（不只是 label），否则富渲染取不到 platform/倍率。
 */

const options: MultiSelectOption[] = [
  { value: 1, label: 'Group Alpha' },
  { value: 2, label: 'Group Beta' },
]

// 富渲染需要的额外字段。MultiSelectOption 只声明了 value/label/disabled，
// 但调用方可以传更宽的对象再在插槽里强转 —— 这正是 KeysView 的用法。
const richOptions = [
  { value: 1, label: 'claude-ccmax', platform: 'anthropic', rate: 1.0 },
  { value: 2, label: 'codex', platform: 'openai', rate: 1.2 },
] as unknown as MultiSelectOption[]

let unmountWrapper: (() => void) | undefined

afterEach(() => {
  unmountWrapper?.()
  unmountWrapper = undefined
  document.body.innerHTML = ''
})

function mountWith(props: Record<string, unknown>, slots?: Record<string, string>) {
  const wrapper = mount(MultiSelect, {
    props: { modelValue: [], options, exclusiveEmptyLabel: 'Global', ...props },
    slots,
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

describe('MultiSelect 插槽（issue #171 新增）', () => {
  it('不传插槽时 trigger 与选项仍是纯文本 label（#170 的用法不得回归）', async () => {
    const wrapper = mountWith({ modelValue: [1, 2] })
    expect(wrapper.get('.select-value').text()).toBe('Group Alpha、Group Beta')

    const dropdown = await openDropdown(wrapper)
    const labels = Array.from(dropdown.querySelectorAll('.select-option-label')).map((el) =>
      el.textContent?.trim(),
    )
    expect(labels).toContain('Group Alpha')
    expect(labels).toContain('Group Beta')
  })

  it('未选中任何项时回退到互斥空选项文案（原有行为）', () => {
    const wrapper = mountWith({ modelValue: [] })
    expect(wrapper.get('.select-value').text()).toBe('Global')
  })

  it('selected 插槽能拿到完整的 option 对象，不只是 label', () => {
    const wrapper = mountWith(
      { modelValue: [1, 2], options: richOptions },
      {
        // 插槽里读 platform —— 只有拿到完整对象才可能渲染出来。
        selected: `
          <template #selected="{ options }">
            <span class="probe">{{ options.map((o) => o.platform).join('|') }}</span>
          </template>
        `,
      },
    )
    expect(wrapper.get('.probe').text()).toBe('anthropic|openai')
  })

  it('selected 插槽收到的顺序跟 modelValue 而不是 options', () => {
    const wrapper = mountWith(
      { modelValue: [2, 1], options: richOptions },
      {
        selected: `
          <template #selected="{ options }">
            <span class="probe">{{ options.map((o) => o.label).join('|') }}</span>
          </template>
        `,
      },
    )
    expect(wrapper.get('.probe').text()).toBe('codex|claude-ccmax')
  })

  it('option 插槽能拿到 option 与 selected 状态', async () => {
    const wrapper = mountWith(
      { modelValue: [2], options: richOptions },
      {
        option: `
          <template #option="{ option, selected }">
            <span class="probe" :data-selected="selected">{{ option.platform }}</span>
          </template>
        `,
      },
    )
    const dropdown = await openDropdown(wrapper)
    const probes = Array.from(dropdown.querySelectorAll<HTMLElement>('.probe'))
    expect(probes.map((el) => el.textContent?.trim())).toEqual(['anthropic', 'openai'])
    // 第二项（codex，value=2）在 modelValue 里，应当是选中态。
    expect(probes[0].dataset.selected).toBe('false')
    expect(probes[1].dataset.selected).toBe('true')
  })

  it('modelValue 里有不存在于 options 的 id 时，selected 插槽跳过它而不是崩', () => {
    const wrapper = mountWith(
      { modelValue: [1, 999], options: richOptions },
      {
        selected: `
          <template #selected="{ options }">
            <span class="probe">{{ options.length }}</span>
          </template>
        `,
      },
    )
    // 悬空 id 不进 selectedOptions；纯文本回退路径仍会用 "#999" 显示，两者互不影响。
    expect(wrapper.get('.probe').text()).toBe('1')
  })
})
