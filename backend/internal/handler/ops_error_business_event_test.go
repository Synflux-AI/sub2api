package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func i64(v int64) *int64 { return &v }
func iptr(v int) *int    { return &v }
func sptr(v string) *string {
	return &v
}
func i16(v int16) *int16 { return &v }

// fullOpsErrorEntry covers every whitelisted dimension plus the payload fields
// that must never be projected.
func fullOpsErrorEntry() *service.OpsInsertErrorLogInput {
	return &service.OpsInsertErrorLogInput{
		RequestID:       "req-def",
		ClientRequestID: "client-ghi",

		UserID:    i64(11),
		APIKeyID:  i64(22),
		AccountID: i64(33),
		GroupID:   i64(44),
		ClientIP:  sptr("203.0.113.7"),

		Platform:         "openai",
		Model:            "gpt-5",
		RequestPath:      "/v1/chat/completions",
		Stream:           true,
		InboundEndpoint:  "/v1/chat/completions",
		UpstreamEndpoint: "/v1/responses",
		RequestedModel:   "gpt-5-mini",
		UpstreamModel:    "gpt-5",
		RequestType:      i16(2),
		UserAgent:        "claude-cli/1.2.3",

		ErrorPhase:        "upstream",
		ErrorType:         "stream_read_error",
		Severity:          "error",
		StatusCode:        502,
		IsBusinessLimited: true,
		IsCountTokens:     false,

		ErrorMessage: "upstream RST_STREAM",

		ErrorSource: "upstream",
		ErrorOwner:  "provider",

		UpstreamStatusCode: iptr(500),

		AuthLatencyMs:      i64(3),
		RoutingLatencyMs:   i64(7),
		UpstreamLatencyMs:  i64(1200),
		ResponseLatencyMs:  i64(1300),
		TimeToFirstTokenMs: i64(450),

		CreatedAt:    time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC),
		APIKeyPrefix: "sk-ant-1",

		// Must never reach OpenObserve.
		ErrorBody:            `{"error":{"message":"user prompt: my private data"}}`,
		UpstreamErrorMessage: sptr("internal error"),
		UpstreamErrorDetail:  sptr("raw upstream body with prompt echo"),
		UpstreamErrorsJSON:   sptr(`[{"upstream_response_body":"secret"}]`),
	}
}

func captureErrorEvent(t *testing.T, entry *service.OpsInsertErrorLogInput, ctx context.Context) (map[string]any, string) {
	t.Helper()

	buf := &bytes.Buffer{}
	restore := logger.SetBusinessEventWriterForTest(buf)
	t.Cleanup(restore)

	emitOpsErrorBusinessEvent(ctx, entry)

	line := strings.TrimSpace(buf.String())
	require.NotEmpty(t, line, "expected one error_log business event")
	require.NotContains(t, line, "\n", "expected exactly one event line")

	var event map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &event))
	return event, line
}

func correlatedGatewayContext() context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-abc")
	ctx = context.WithValue(ctx, ctxkey.RequestID, "req-def")
	return context.WithValue(ctx, ctxkey.ClientRequestID, "client-ghi")
}

func TestOpsErrorBusinessEventProjectsSearchDimensions(t *testing.T) {
	event, _ := captureErrorEvent(t, fullOpsErrorEntry(), correlatedGatewayContext())

	require.Equal(t, logger.BusinessEventKindErrorLog, event["event_kind"])
	require.EqualValues(t, 1, event["event_schema_version"])
	require.Equal(t, "trace-abc", event["trace_id"])
	require.Equal(t, true, event["ops_system_log_skip"])
	require.Equal(t, "2026-07-29T06:00:00Z", event["db_created_at"])

	// The field this issue exists for.
	require.Equal(t, "claude-cli/1.2.3", event["user_agent"])

	for key, want := range map[string]any{
		"user_id":                float64(11),
		"api_key_id":             float64(22),
		"account_id":             float64(33),
		"group_id":               float64(44),
		"api_key_prefix":         "sk-ant-1",
		"client_ip":              "203.0.113.7",
		"platform":               "openai",
		"model":                  "gpt-5",
		"requested_model":        "gpt-5-mini",
		"upstream_model":         "gpt-5",
		"request_path":           "/v1/chat/completions",
		"inbound_endpoint":       "/v1/chat/completions",
		"upstream_endpoint":      "/v1/responses",
		"request_type":           float64(2),
		"stream":                 true,
		"error_phase":            "upstream",
		"error_type":             "stream_read_error",
		"severity":               "error",
		"status_code":            float64(502),
		"upstream_status_code":   float64(500),
		"is_business_limited":    true,
		"error_source":           "upstream",
		"error_owner":            "provider",
		"error_message":          "upstream RST_STREAM",
		"auth_latency_ms":        float64(3),
		"routing_latency_ms":     float64(7),
		"upstream_latency_ms":    float64(1200),
		"response_latency_ms":    float64(1300),
		"time_to_first_token_ms": float64(450),
	} {
		require.Equal(t, want, event[key], "field %q", key)
	}
}

// The sanitizer upstream of this emitter strips credentials, not prompts or PII,
// so "already sanitized" must not be read as "safe to copy across stores".
func TestOpsErrorBusinessEventNeverProjectsErrorBodies(t *testing.T) {
	_, line := captureErrorEvent(t, fullOpsErrorEntry(), correlatedGatewayContext())

	for _, forbidden := range []string{
		"error_body",
		"upstream_error_detail",
		"upstream_errors",
		"upstream_error_message",
		"my private data",
		"raw upstream body with prompt echo",
		"secret",
	} {
		require.NotContains(t, line, forbidden, "error bodies must stay in Postgres only")
	}
}

func TestOpsErrorBusinessEventHasNoDuplicateKeys(t *testing.T) {
	_, line := captureErrorEvent(t, fullOpsErrorEntry(), correlatedGatewayContext())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(line), &raw))
	for key := range raw {
		require.Equal(t, 1, strings.Count(line, `"`+key+`":`), "duplicate JSON key %q", key)
	}
}

// ops_error_logs.request_id is the same request-scoped id the envelope carries
// (the handler reads it straight off the request context), so it must not be
// emitted a second time under another name.
func TestOpsErrorBusinessEventDoesNotDuplicateRequestID(t *testing.T) {
	event, _ := captureErrorEvent(t, fullOpsErrorEntry(), correlatedGatewayContext())

	require.Equal(t, "req-def", event["request_id"])
	require.NotContains(t, event, "error_request_id")
	require.NotContains(t, event, "ops_request_id")
}

func TestOpsErrorBusinessEventOmitsEmptyOptionalFields(t *testing.T) {
	entry := &service.OpsInsertErrorLogInput{
		ErrorPhase: "auth",
		ErrorType:  "invalid_api_key",
		StatusCode: 401,
		CreatedAt:  time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC),
	}

	event, _ := captureErrorEvent(t, entry, context.Background())

	for _, key := range []string{
		"user_id", "api_key_id", "account_id", "group_id", "api_key_prefix",
		"client_ip", "platform", "model", "requested_model", "upstream_model",
		"user_agent", "severity", "upstream_status_code", "auth_latency_ms",
		"trace_id", "request_id", "client_request_id",
	} {
		require.NotContains(t, event, key, "empty %q must be omitted, not emitted as a zero value", key)
	}
	require.Equal(t, "invalid_api_key", event["error_type"])
	require.Equal(t, float64(401), event["status_code"])
}

func TestOpsErrorBusinessEventTruncatesUserAgentAndMessage(t *testing.T) {
	entry := fullOpsErrorEntry()
	entry.UserAgent = strings.Repeat("u", opsErrorLogMaxUserAgentBytes+200)
	entry.ErrorMessage = strings.Repeat("m", opsBusinessEventMaxErrorMessageBytes+500)

	event, _ := captureErrorEvent(t, entry, correlatedGatewayContext())

	require.Len(t, event["user_agent"], opsErrorLogMaxUserAgentBytes)
	require.Len(t, event["error_message"], opsBusinessEventMaxErrorMessageBytes)
}

func TestOpsErrorBusinessEventSkippedWhenDisabled(t *testing.T) {
	buf := &bytes.Buffer{}
	restore := logger.SetBusinessEventWriterForTest(buf)
	t.Cleanup(restore)
	logger.SetBusinessEventsEnabled(false)

	emitOpsErrorBusinessEvent(correlatedGatewayContext(), fullOpsErrorEntry())

	require.Empty(t, buf.String())
}
