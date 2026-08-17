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
	out := sanitizeUpstreamErrorPayload(`token: "abcdefghij" trailing`)
	require.Equal(t, `token: "`+upstreamSensitiveMask+`" trailing`, out)
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
