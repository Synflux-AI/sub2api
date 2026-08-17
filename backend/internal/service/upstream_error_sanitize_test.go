package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeUpstreamErrorPayloadMasksJSONFields(t *testing.T) {
	in := `{"error":{"message":"bad key","api_key":"sk-live-1234567890","nested":{"authorization":"Bearer abcdefghij"}}}`
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "sk-live-1234567890")
	require.NotContains(t, out, "abcdefghij")
	require.Contains(t, out, upstreamSensitiveMask)
	require.Contains(t, out, "bad key")
}

func TestSanitizeUpstreamErrorPayloadMasksSSEDataLines(t *testing.T) {
	in := "event: error\n" + `data: {"type":"error","error":{"message":"denied","password":"hunter2hunter2"}}` + "\n"
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "hunter2hunter2")
	require.Contains(t, out, "event: error")
	require.Contains(t, out, "denied")
}

func TestSanitizeUpstreamErrorPayloadMasksPlainTextSecrets(t *testing.T) {
	in := `upstream rejected: api_key=sk-live-abcdefgh, Authorization: Bearer tok-0123456789 (see https://x.test/v1?access_token=zzzzzzzz)`
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "sk-live-abcdefgh")
	require.NotContains(t, out, "tok-0123456789")
	require.NotContains(t, out, "zzzzzzzz")
	require.Contains(t, out, "upstream rejected")
}

func TestSanitizeUpstreamErrorPayloadKeepsSignatureText(t *testing.T) {
	in := "Invalid `signature` in `thinking` block"
	require.Equal(t, in, sanitizeUpstreamErrorPayload(in))
}

func TestSanitizeUpstreamErrorPayloadPreservesQuotingInText(t *testing.T) {
	// 原用例使用裸 "token" 作为 key 验证引号保留机制；task-1 fix round 1
	// 裁决要求文本 KV 正则移除裸 token（避免对客文案里的 "token: 4096"
	// 计数误打码，见 upstream_error_sanitize.go 里 upstreamSensitiveKVRegex
	// 的注释）。改用 "password" 验证同样的引号保留机制，不影响测试意图。
	out := sanitizeUpstreamErrorPayload(`password: "abcdefghij" trailing`)
	require.Equal(t, `password: "`+upstreamSensitiveMask+`" trailing`, out)
}

func TestSanitizeUpstreamErrorMessageStillMasksQueryParams(t *testing.T) {
	out := sanitizeUpstreamErrorMessage("call https://x.test/v1?key=secretvalue failed")
	require.NotContains(t, out, "secretvalue")
	require.True(t, strings.Contains(out, upstreamSensitiveMask))
}

func TestSanitizeUpstreamErrorPayloadEmptyInputUnchanged(t *testing.T) {
	require.Equal(t, "", sanitizeUpstreamErrorPayload(""))
	require.Equal(t, "   ", sanitizeUpstreamErrorPayload("   "))
}

// TestSanitizeUpstreamErrorPayloadMasksTruncatedJSONField 覆盖 Finding 1
// 的第一个复现，及 Finding 2 的叠加论证：既有 40+ 调用点在 truncateString
// 之后才赋给 ev.Detail，截断可能把合法 JSON 切成非法 JSON、落到文本兜底
// 路径。这里验证被截断成非法 JSON 的 api_key 字段仍会被掩码。
func TestSanitizeUpstreamErrorPayloadMasksTruncatedJSONField(t *testing.T) {
	in := `{"error":{"message":"bad key","api_key":"sk-live-1234567890abcdef`
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "sk-live-1234567890abcdef")
	require.Contains(t, out, upstreamSensitiveMask)
	require.Contains(t, out, "bad key")
}

// TestSanitizeUpstreamErrorPayloadMasksJSONWithEmbeddedDataColon 覆盖
// Finding 1 的第二个复现：单行合法 JSON 的字符串字段里恰好出现子串
// "data:"，isLikelySSEPayload 若只做 substring 判断会把整个 payload
// 误路由进 SSE 逐行文本路径，导致同一对象里的 api_key 原样泄漏。
func TestSanitizeUpstreamErrorPayloadMasksJSONWithEmbeddedDataColon(t *testing.T) {
	in := `{"error":{"message":"authentication data: invalid","api_key":"sk-live-999888777"}}`
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "sk-live-999888777")
	require.Contains(t, out, upstreamSensitiveMask)
	require.Contains(t, out, "authentication data: invalid")
}

// TestSanitizeUpstreamErrorPayloadMasksCompactJSONTextFallback 验证紧凑
// （无空格）JSON 走文本兜底路径时，quote-aware 的 upstreamSensitiveKVRegex
// 依然能匹配 "key":"value" 形态。
func TestSanitizeUpstreamErrorPayloadMasksCompactJSONTextFallback(t *testing.T) {
	out := maskTextSecrets(`{"password":"hunter2hunter2"}`)

	require.NotContains(t, out, "hunter2hunter2")
	require.Contains(t, out, upstreamSensitiveMask)
}

// TestSanitizeUpstreamErrorPayloadKeepsBareTokenCountText 覆盖 Finding 3
// 的裁决：文本 KV 正则移除裸 token，避免把 "token: 4096" 这类合法的模型
// token 计数文案误打码——这类文案可能经 sanitizeUpstreamErrorMessage 直接
// 进入面向客户端的错误响应（如 openai_images_responses.go 的
// OpenAIImagesUpstreamError.Message）。同时确认 JSON 字段名清单里的
// token 仍会被掩码（该字段名锚定、语义无歧义）。
func TestSanitizeUpstreamErrorPayloadKeepsBareTokenCountText(t *testing.T) {
	require.Equal(t, "token: 4096", sanitizeUpstreamErrorPayload("token: 4096"))
	require.Equal(t, "token=4096", sanitizeUpstreamErrorPayload("token=4096"))

	out := sanitizeUpstreamErrorPayload(`{"token":"secret"}`)
	require.NotContains(t, out, "secret")
	require.Contains(t, out, upstreamSensitiveMask)
}

// TestSanitizeUpstreamErrorPayloadMasksCookieInTruncatedJSON 覆盖 Finding 4：
// log_upstream_error_body_max_bytes 默认 2048，上游 >2KB 的错误正文截断后
// 落在对象中间，产出非法 JSON，走文本兜底路径（maskTextSecrets）。此前
// upstreamSensitiveKVRegex 缺了 cookie/session_id/id_token 三个字段名，
// session cookie 会原样写进 ops_error_logs 与 stdout。这里用 finding 描述
// 里给出的确切复现串验证三者都被掩码。
func TestSanitizeUpstreamErrorPayloadMasksCookieInTruncatedJSON(t *testing.T) {
	in := `{"error":{"message":"auth failed"},"debug":{"cookie":"sk_session=abc123`
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "sk_session=abc123")
	require.Contains(t, out, upstreamSensitiveMask)
	require.Contains(t, out, "auth failed")
}

// TestSanitizeUpstreamErrorPayloadMasksSessionIDAndIDTokenInText 补齐
// Finding 4 的另外两个遗漏字段：session_id、id_token，同样走文本兜底路径。
func TestSanitizeUpstreamErrorPayloadMasksSessionIDAndIDTokenInText(t *testing.T) {
	out := sanitizeUpstreamErrorPayload(`session_id=abcdef123456 id_token=eyJhbGciOiJIUzI1NiJ9.trailing`)
	require.NotContains(t, out, "abcdef123456")
	require.NotContains(t, out, "eyJhbGciOiJIUzI1NiJ9.trailing")
	require.Contains(t, out, upstreamSensitiveMask)
}
