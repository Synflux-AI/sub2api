/**
 * 计算缓存命中率（%）
 * 公式：cache_read / (input + cache_read + cache_creation)
 * Claude 和 OpenAI 都已在存库前将 input_tokens 排除缓存部分，公式统一。
 */
export function calcCacheHitRate(row: {
  input_tokens: number
  cache_read_tokens: number
  cache_creation_tokens: number
}): number | null {
  const total = row.input_tokens + row.cache_read_tokens + row.cache_creation_tokens
  if (total <= 0 || row.cache_read_tokens <= 0) return null
  return (row.cache_read_tokens / total) * 100
}

/**
 * 格式化缓存 token 数量（1K/1M 缩写）
 */
export function formatCacheTokens(tokens: number): string {
  if (tokens >= 1000000) return `${(tokens / 1000000).toFixed(1)}M`
  if (tokens >= 1000) return `${(tokens / 1000).toFixed(1)}K`
  return tokens.toLocaleString()
}

/**
 * 自适应精度格式化倍率：保留至多 4 位小数并去掉末尾多余的 0，
 * 但至少保留 2 位小数（0.035 -> "0.035"，0.3 -> "0.30"，1 -> "1.00"）
 */
export function formatMultiplier(val: number): string {
  if (val < 0.0001) return val.toPrecision(2)
  return val.toFixed(4).replace(/(\.\d{2}\d*?)0+$/, '$1')
}
