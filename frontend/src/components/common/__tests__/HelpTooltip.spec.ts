import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'

enableAutoUnmount(afterEach)

function getTooltipElement(): HTMLDivElement {
  const tooltip = document.body.querySelector('[role="tooltip"]')
  if (!(tooltip instanceof HTMLDivElement)) {
    throw new Error('tooltip element not found')
  }
  return tooltip
}

describe('HelpTooltip', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    document.body.innerHTML = ''
    // jsdom 没实现 scrollTo，直接还原被本文件改写过的滚动位移
    Object.defineProperty(window, 'scrollY', { value: 0, configurable: true })
    Object.defineProperty(window, 'scrollX', { value: 0, configurable: true })
  })

  it('keeps the existing hover interaction by default', async () => {
    const wrapper = mount(HelpTooltip, {
      attachTo: document.body,
      props: {
        content: 'hover details',
      },
    })

    const trigger = wrapper.get('.group')
    const tooltip = getTooltipElement()

    expect(tooltip.style.display).toBe('none')

    await trigger.trigger('mouseenter')
    await nextTick()
    expect(tooltip.style.display).not.toBe('none')

    await trigger.trigger('mouseleave')
    await nextTick()
    expect(tooltip.style.display).toBe('none')
  })

  // 气泡是 position:fixed，getBoundingClientRect 已经是视口坐标；再叠 scrollY 会把气泡
  // 整体下移一个滚动距离，正好压住鼠标所在的触发点 → trigger 收到 mouseleave 立刻隐藏，
  // 隐藏后鼠标又落回 trigger → 一闪一闪的死循环。
  it('positions the fixed tooltip in viewport coordinates when the page is scrolled', async () => {
    const wrapper = mount(HelpTooltip, {
      attachTo: document.body,
      props: {
        content: 'scrolled details',
      },
    })

    const trigger = wrapper.get('.group')
    trigger.element.getBoundingClientRect = () =>
      ({ top: 500, left: 200, width: 40, height: 20, bottom: 520, right: 240, x: 200, y: 500 }) as DOMRect
    Object.defineProperty(window, 'scrollY', { value: 400, configurable: true })
    Object.defineProperty(window, 'scrollX', { value: 30, configurable: true })

    await trigger.trigger('mouseenter')
    await nextTick()

    const tooltip = getTooltipElement()
    expect(tooltip.style.top).toBe('492px')
    expect(tooltip.style.left).toBe('220px')

    wrapper.unmount()
  })

  it('keeps a hover tooltip open while the pointer moves between the trigger and the tooltip', async () => {
    const wrapper = mount(HelpTooltip, {
      attachTo: document.body,
      props: {
        content: 'copyable details',
      },
    })

    const trigger = wrapper.get('.group')
    const tooltip = getTooltipElement()

    await trigger.trigger('mouseenter')
    await nextTick()
    expect(tooltip.style.display).not.toBe('none')

    await trigger.trigger('mouseleave', { relatedTarget: tooltip })
    await nextTick()
    expect(tooltip.style.display).not.toBe('none')

    tooltip.dispatchEvent(new MouseEvent('mouseleave', { relatedTarget: trigger.element }))
    await nextTick()
    expect(tooltip.style.display).not.toBe('none')

    tooltip.dispatchEvent(new MouseEvent('mouseleave', { relatedTarget: null }))
    await nextTick()
    expect(tooltip.style.display).toBe('none')
  })

  it('supports click-to-toggle details and closes on outside click', async () => {
    const wrapper = mount(HelpTooltip, {
      attachTo: document.body,
      props: {
        content: 'click details',
        trigger: 'click',
      },
    })

    const trigger = wrapper.get('.group')
    const tooltip = getTooltipElement()

    expect(tooltip.style.display).toBe('none')

    await trigger.trigger('click')
    await nextTick()
    expect(tooltip.style.display).not.toBe('none')
    expect(tooltip.textContent).toContain('click details')

    const closeButton = tooltip.querySelector('button[aria-label="Close"]')
    if (!(closeButton instanceof HTMLButtonElement)) {
      throw new Error('close button not found')
    }
    closeButton.click()
    await nextTick()
    expect(tooltip.style.display).toBe('none')

    await trigger.trigger('click')
    await nextTick()
    expect(tooltip.style.display).not.toBe('none')

    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()
    expect(tooltip.style.display).toBe('none')
  })

  it.each([
    { scrollX: 120, scrollY: 0 },
    { scrollX: 0, scrollY: 240 },
    { scrollX: 120, scrollY: 240 },
  ])('uses viewport coordinates when opening with scroll ($scrollX, $scrollY)', async ({ scrollX, scrollY }) => {
    vi.stubGlobal('scrollX', scrollX)
    vi.stubGlobal('scrollY', scrollY)
    const wrapper = mount(HelpTooltip, {
      attachTo: document.body,
      props: { content: 'positioned details' },
    })
    const trigger = wrapper.get('.group')
    vi.spyOn(trigger.element, 'getBoundingClientRect').mockReturnValue(new DOMRect(80, 160, 24, 24))

    await trigger.trigger('mouseenter')
    await nextTick()

    const tooltip = getTooltipElement()
    expect(tooltip.style.display).not.toBe('none')
    expect(tooltip.classList.contains('fixed')).toBe(true)
    expect(tooltip.style.left).toBe('92px')
    expect(tooltip.style.top).toBe('152px')
  })

  it('keeps the tooltip inside a narrow mobile viewport', async () => {
    vi.stubGlobal('innerWidth', 375)
    const wrapper = mount(HelpTooltip, {
      attachTo: document.body,
      props: { content: 'edge details' },
    })
    const trigger = wrapper.get('.group')
    const tooltip = getTooltipElement()
    // 图标贴在视口左侧、靠近顶部：居中会向左溢出，上方也放不下
    vi.spyOn(trigger.element, 'getBoundingClientRect').mockReturnValue(new DOMRect(20, 40, 16, 16))
    vi.spyOn(tooltip, 'getBoundingClientRect').mockReturnValue(new DOMRect(0, 0, 256, 80))

    await trigger.trigger('mouseenter')
    await nextTick()

    expect(tooltip.style.left).toBe('8px')
    expect(tooltip.style.top).toBe('64px')
  })

  it.each(['window scroll', 'nested scroll', 'resize'] as const)(
    'recalculates viewport coordinates for an open tooltip on %s',
    async (change) => {
      const container = document.createElement('div')
      document.body.appendChild(container)
      const wrapper = mount(HelpTooltip, {
        attachTo: container,
        props: { content: 'moving details', trigger: 'click' },
      })
      const trigger = wrapper.get('.group')
      const getBoundingClientRect = vi.spyOn(trigger.element, 'getBoundingClientRect')
        .mockReturnValue(new DOMRect(80, 160, 24, 24))

      await trigger.trigger('click')
      await nextTick()

      const tooltip = getTooltipElement()
      expect(tooltip.style.left).toBe('92px')
      expect(tooltip.style.top).toBe('152px')

      vi.stubGlobal('scrollX', 120)
      vi.stubGlobal('scrollY', 240)
      getBoundingClientRect.mockReturnValue(new DOMRect(40, 100, 32, 24))

      if (change === 'nested scroll') {
        container.dispatchEvent(new Event('scroll', { bubbles: false }))
      } else {
        window.dispatchEvent(new Event(change === 'resize' ? 'resize' : 'scroll'))
      }
      await nextTick()

      expect(tooltip.style.display).not.toBe('none')
      expect(getBoundingClientRect).toHaveBeenCalledTimes(2)
      expect(tooltip.style.left).toBe('56px')
      expect(tooltip.style.top).toBe('92px')
    },
  )
})
