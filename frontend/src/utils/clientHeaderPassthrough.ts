// 客户端请求头透传（#300）：全局白名单 + 账号级黑名单的前端校验。
// 口径与后端 clientHeaderPassthroughBlockedNames / NormalizeClientHeaderPassthroughAllowlist 一致。

export const MAX_CLIENT_HEADER_PASSTHROUGH_ENTRIES = 64
export const MAX_CLIENT_HEADER_PASSTHROUGH_NAME_LENGTH = 200

// 目前接入了透传的平台；账号编辑页只对这些平台展示「禁止透传的请求头」。
export const CLIENT_HEADER_PASSTHROUGH_PLATFORMS: readonly string[] = ['gemini']

export const CLIENT_HEADER_PASSTHROUGH_PROTECTED_NAMES: ReadonlySet<string> = new Set([
  'host',
  'content-length',
  'content-type',
  'content-encoding',
  'transfer-encoding',
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'proxy-connection',
  'te',
  'trailer',
  'trailers',
  'upgrade',
  'expect',
  'authorization',
  'x-api-key',
  'x-goog-api-key',
  'cookie',
  'accept-encoding',
  'sec-websocket-key',
  'sec-websocket-version',
  'sec-websocket-extensions',
  'sec-websocket-protocol',
  'sec-websocket-accept',
  'x-trace-id'
])

// RFC 7230 token
const HEADER_NAME_PATTERN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/

export type ClientHeaderPassthroughNamesResult =
  | { ok: true; names: string[] }
  | { ok: false; reason: 'invalid' | 'protected' | 'too_many'; name?: string }

// 按行（也接受逗号）拆分请求头名称，规范化交给 normalizeClientHeaderPassthroughNames。
export function splitClientHeaderNames(input: string): string[] {
  return input.split(/[\n,]/).map((name) => name.trim()).filter(Boolean)
}

export function normalizeClientHeaderPassthroughNames(
  names: readonly string[],
  options: { rejectProtected: boolean }
): ClientHeaderPassthroughNamesResult {
  const out: string[] = []
  for (const raw of names) {
    const name = raw.trim().toLowerCase()
    if (!name || out.includes(name)) continue
    if (name.length > MAX_CLIENT_HEADER_PASSTHROUGH_NAME_LENGTH || !HEADER_NAME_PATTERN.test(name)) {
      return { ok: false, reason: 'invalid', name }
    }
    if (options.rejectProtected && CLIENT_HEADER_PASSTHROUGH_PROTECTED_NAMES.has(name)) {
      return { ok: false, reason: 'protected', name }
    }
    if (out.length >= MAX_CLIENT_HEADER_PASSTHROUGH_ENTRIES) {
      return { ok: false, reason: 'too_many' }
    }
    out.push(name)
  }
  return { ok: true, names: out }
}

export function readClientHeaderNameList(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  return value.filter((item): item is string => typeof item === 'string')
}
