import { describe, expect, it } from 'vitest'
import {
  MAX_CLIENT_HEADER_PASSTHROUGH_ENTRIES,
  normalizeClientHeaderPassthroughNames,
  readClientHeaderNameList,
  splitClientHeaderNames
} from '../clientHeaderPassthrough'

describe('clientHeaderPassthrough', () => {
  it('按行与逗号拆分并规范化为小写去重', () => {
    const names = splitClientHeaderNames(' X-Session-Id \nx-session-id,\n\nSession_Id , X-Custom')
    expect(normalizeClientHeaderPassthroughNames(names, { rejectProtected: true })).toEqual({
      ok: true,
      names: ['x-session-id', 'session_id', 'x-custom']
    })
  })

  it('全局白名单拒绝受保护头，账号黑名单允许', () => {
    for (const name of ['Authorization', 'x-api-key', 'X-Goog-Api-Key', 'Cookie', 'Host', 'Content-Type', 'Accept-Encoding', 'X-Trace-Id', 'Transfer-Encoding']) {
      expect(normalizeClientHeaderPassthroughNames([name], { rejectProtected: true })).toEqual({
        ok: false,
        reason: 'protected',
        name: name.toLowerCase()
      })
      expect(normalizeClientHeaderPassthroughNames([name], { rejectProtected: false }).ok).toBe(true)
    }
  })

  it('拒绝非法名称与超出上限', () => {
    expect(normalizeClientHeaderPassthroughNames(['bad header'], { rejectProtected: false })).toEqual({
      ok: false,
      reason: 'invalid',
      name: 'bad header'
    })
    const tooMany = Array.from({ length: MAX_CLIENT_HEADER_PASSTHROUGH_ENTRIES + 1 }, (_, i) => `x-h-${i}`)
    expect(normalizeClientHeaderPassthroughNames(tooMany, { rejectProtected: true })).toEqual({ ok: false, reason: 'too_many' })
  })

  it('只读取字符串数组', () => {
    expect(readClientHeaderNameList(['a', 1, 'b'])).toEqual(['a', 'b'])
    expect(readClientHeaderNameList('a')).toEqual([])
    expect(readClientHeaderNameList(undefined)).toEqual([])
  })
})
