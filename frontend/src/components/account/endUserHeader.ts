// 终端用户 ID 头（#342）：账号 extra 字段的读写辅助。
// 后端校验见 backend/internal/service/end_user_header.go（NormalizeEndUserHeaderExtra）。

export const END_USER_HEADER_ENABLED_KEY = 'end_user_header_enabled'
export const END_USER_HEADER_NAME_KEY = 'end_user_header_name'
export const END_USER_HEADER_PREFIX_KEY = 'end_user_header_prefix'

export const DEFAULT_END_USER_HEADER_NAME = 'X-End-User'
export const END_USER_FORWARD_HEADER = 'X-Sub2api-End-User'

export interface EndUserHeaderState {
  enabled: boolean
  name: string
  prefix: string
}

export function emptyEndUserHeaderState(): EndUserHeaderState {
  return { enabled: false, name: '', prefix: '' }
}

export function readEndUserHeaderState(extra: unknown): EndUserHeaderState {
  const record = (extra && typeof extra === 'object' ? extra : {}) as Record<string, unknown>
  const name = record[END_USER_HEADER_NAME_KEY]
  const prefix = record[END_USER_HEADER_PREFIX_KEY]
  return {
    enabled: record[END_USER_HEADER_ENABLED_KEY] === true,
    name: typeof name === 'string' ? name : '',
    prefix: typeof prefix === 'string' ? prefix : ''
  }
}

const END_USER_KEYS = [END_USER_HEADER_ENABLED_KEY, END_USER_HEADER_NAME_KEY, END_USER_HEADER_PREFIX_KEY]

/**
 * 把表单状态写回 extra。关闭时删除全部相关 key（而非写 false），开启时只写非空的头名 / 前缀。
 * 返回 null 表示 extra 无需变化（未开启且原本不含相关 key），调用方据此避免凭空加上 extra。
 */
export function applyEndUserHeaderExtra(
  extra: Record<string, unknown> | undefined | null,
  state: EndUserHeaderState
): Record<string, unknown> | null {
  const current = extra || {}
  const hasExisting = END_USER_KEYS.some((key) => key in current)
  if (!state.enabled && !hasExisting) return null

  const next: Record<string, unknown> = { ...current }
  for (const key of END_USER_KEYS) delete next[key]
  if (!state.enabled) return next

  next[END_USER_HEADER_ENABLED_KEY] = true
  const name = state.name.trim()
  if (name) next[END_USER_HEADER_NAME_KEY] = name
  const prefix = state.prefix.trim()
  if (prefix) next[END_USER_HEADER_PREFIX_KEY] = prefix
  return next
}
