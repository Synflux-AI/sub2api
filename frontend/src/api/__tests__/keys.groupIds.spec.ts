import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post, put } = vi.hoisted(() => ({ post: vi.fn(), put: vi.fn() }))

vi.mock('../client', () => ({
  apiClient: {
    get: vi.fn(),
    post,
    put,
    delete: vi.fn(),
  },
}))

import { create, update } from '../keys'

/**
 * issue #171 的线格式契约：group_ids 是**三态**的。
 *
 * 这一层最容易出的错是把「显式空数组」优化掉：`if (groupIds?.length)` 之类的写法会
 * 让用户点「清空分组」时请求里根本不带 group_ids，后端于是回退到旧的 group_id
 * 兼容路径，把分组静默改回原来的默认组。所以必须区分 undefined 与 []。
 */
describe('keysAPI 的 group_ids 线格式', () => {
  beforeEach(() => {
    post.mockReset().mockResolvedValue({ data: {} })
    put.mockReset().mockResolvedValue({ data: {} })
  })

  it('传了 groupIds 时只发 group_ids，不发 group_id', async () => {
    await create('k', 10, undefined, undefined, undefined, undefined, undefined, undefined, [10, 20])

    const payload = post.mock.calls[0][1]
    expect(payload.group_ids).toEqual([10, 20])
    // 同时发两个时后端要求 group_id 必须属于 group_ids；默认组本该由后端按稳定规则
    // 解析，前端不该猜，所以这里刻意不发。
    expect('group_id' in payload).toBe(false)
  })

  it('groupIds 是空数组时**仍要发**该字段（显式解绑）', async () => {
    await create('k', undefined, undefined, undefined, undefined, undefined, undefined, undefined, [])

    const payload = post.mock.calls[0][1]
    expect('group_ids' in payload).toBe(true)
    expect(payload.group_ids).toEqual([])
    expect('group_id' in payload).toBe(false)
  })

  it('不传 groupIds 时走 group_id 兼容路径（旧调用方不受影响）', async () => {
    await create('k', 7)

    const payload = post.mock.calls[0][1]
    expect(payload.group_id).toBe(7)
    expect('group_ids' in payload).toBe(false)
  })

  it('groupId 与 groupIds 都不传时两个字段都不发', async () => {
    await create('k')

    const payload = post.mock.calls[0][1]
    expect('group_id' in payload).toBe(false)
    expect('group_ids' in payload).toBe(false)
  })

  it('create 传入的数组被拷贝，调用方之后改动它不影响已发出的载荷', async () => {
    const ids = [10, 20]
    await create('k', undefined, undefined, undefined, undefined, undefined, undefined, undefined, ids)
    ids.push(30)

    expect(post.mock.calls[0][1].group_ids).toEqual([10, 20])
  })

  it('update 原样透传 group_ids，包括空数组', async () => {
    await update(1, { name: 'n', group_ids: [] })
    expect(put.mock.calls[0][1].group_ids).toEqual([])

    await update(1, { name: 'n', group_ids: [5] })
    expect(put.mock.calls[1][1].group_ids).toEqual([5])
  })
})
