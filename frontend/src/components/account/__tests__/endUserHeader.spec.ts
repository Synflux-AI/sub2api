import { describe, expect, it } from 'vitest'
import { applyEndUserHeaderExtra, readEndUserHeaderState } from '../endUserHeader'

describe('endUserHeader extra helpers', () => {
  it('未开启且原本没有相关 key 时不改动 extra', () => {
    expect(applyEndUserHeaderExtra({ other: 1 }, { enabled: false, name: 'X', prefix: 'a:' })).toBeNull()
    expect(applyEndUserHeaderExtra(undefined, { enabled: false, name: '', prefix: '' })).toBeNull()
  })

  it('开启时写入开关与非空的头名 / 前缀', () => {
    expect(applyEndUserHeaderExtra({ other: 1 }, { enabled: true, name: ' X-Sub2api-End-User ', prefix: '' })).toEqual({
      other: 1,
      end_user_header_enabled: true,
      end_user_header_name: 'X-Sub2api-End-User'
    })
  })

  it('关闭时删除全部相关 key', () => {
    expect(
      applyEndUserHeaderExtra(
        { other: 1, end_user_header_enabled: true, end_user_header_name: 'X-End-User', end_user_header_prefix: 'a:' },
        { enabled: false, name: 'X-End-User', prefix: 'a:' }
      )
    ).toEqual({ other: 1 })
  })

  it('从 extra 回填表单状态', () => {
    expect(readEndUserHeaderState({ end_user_header_enabled: true, end_user_header_prefix: 'a:' })).toEqual({
      enabled: true,
      name: '',
      prefix: 'a:'
    })
    expect(readEndUserHeaderState(null)).toEqual({ enabled: false, name: '', prefix: '' })
  })
})
