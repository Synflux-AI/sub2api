package service

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

func usageStr(v string) *string   { return &v }
func usageI64(v int64) *int64     { return &v }
func usageInt(v int) *int         { return &v }
func usageF64(v float64) *float64 { return &v }

// fullUsageLog populates every field that reaches a usage_logs column, so the
// schema snapshot below sees the widest possible event.
func fullUsageLog() *UsageLog {
	return &UsageLog{
		ID:        99,
		UserID:    11,
		APIKeyID:  22,
		AccountID: 33,
		RequestID: "client:abc-123",

		Model:             "gpt-5",
		RequestedModel:    "gpt-5-mini",
		UpstreamModel:     usageStr("gpt-5"),
		ChannelID:         usageI64(7),
		ModelMappingChain: usageStr("gpt-5-mini→gpt-5"),
		BillingTier:       usageStr("standard"),
		BillingMode:       usageStr("token"),
		ServiceTier:       usageStr("priority"),
		ReasoningEffort:   usageStr("high"),
		InboundEndpoint:   usageStr("/v1/chat/completions"),
		UpstreamEndpoint:  usageStr("/v1/responses"),

		GroupID:        usageI64(44),
		SubscriptionID: usageI64(55),

		InputTokens:         100,
		OutputTokens:        200,
		CacheCreationTokens: 300,
		CacheReadTokens:     400,

		CacheCreation5mTokens: 10,
		CacheCreation1hTokens: 20,

		ImageInputTokens:  5,
		ImageInputCost:    0.01,
		ImageOutputTokens: 6,
		ImageOutputCost:   0.02,

		InputCost:                 1.5,
		OutputCost:                2.5,
		CacheCreationCost:         0.5,
		CacheReadCost:             0.25,
		TotalCost:                 4.75,
		ActualCost:                4.5,
		RateMultiplier:            1.2,
		LongContextBillingApplied: true,
		AccountRateMultiplier:     usageF64(0.8),
		AccountStatsCost:          usageF64(3.8),

		BillingType:  1,
		RequestType:  RequestTypeStream,
		Stream:       true,
		OpenAIWSMode: false,
		DurationMs:   usageInt(1200),
		FirstTokenMs: usageInt(450),
		UserAgent:    usageStr("claude-cli/1.2.3"),
		IPAddress:    usageStr("203.0.113.7"),
		SessionID:    usageStr("sess-xyz"),

		CacheTTLOverridden: true,

		ImageCount:      2,
		ImageSize:       usageStr("1024x1024"),
		ImageInputSize:  usageStr("512x512"),
		ImageOutputSize: usageStr("1024x1024"),
		ImageSizeSource: usageStr("request"),

		VideoCount:           1,
		VideoResolution:      usageStr("720p"),
		VideoDurationSeconds: usageInt(8),

		CreatedAt: time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC),
	}
}

func captureUsageEvent(t *testing.T, log *UsageLog, ctx context.Context) (map[string]any, string) {
	t.Helper()

	buf := &bytes.Buffer{}
	restore := logger.SetBusinessEventWriterForTest(buf)
	t.Cleanup(restore)

	emitUsageBusinessEvent(ctx, log)

	line := strings.TrimSpace(buf.String())
	require.NotEmpty(t, line, "expected one usage_log business event")
	require.NotContains(t, line, "\n", "expected exactly one event line")

	var event map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &event))
	return event, line
}

func correlatedUsageContext() context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-abc")
	ctx = context.WithValue(ctx, ctxkey.RequestID, "req-def")
	return context.WithValue(ctx, ctxkey.ClientRequestID, "client-ghi")
}

// usage_logs.request_id is a billing/idempotency key ("client:..." / "local:..."),
// not the request-scoped id the envelope carries. Emitting it as request_id
// would collide with the envelope and make both unusable.
func TestUsageBusinessEventRenamesBillingRequestID(t *testing.T) {
	event, _ := captureUsageEvent(t, fullUsageLog(), correlatedUsageContext())

	require.Equal(t, "client:abc-123", event["usage_request_id"])
	require.Equal(t, "req-def", event["request_id"])
	require.Equal(t, "trace-abc", event["trace_id"])
	require.Equal(t, "client-ghi", event["client_request_id"])
}

func TestUsageBusinessEventProjectsSearchDimensions(t *testing.T) {
	event, _ := captureUsageEvent(t, fullUsageLog(), correlatedUsageContext())

	require.Equal(t, logger.BusinessEventKindUsageLog, event["event_kind"])
	require.EqualValues(t, 1, event["event_schema_version"])
	require.Equal(t, true, event["ops_system_log_skip"])
	require.Equal(t, "2026-07-29T06:00:00Z", event["db_created_at"])
	require.Equal(t, float64(99), event["db_id"])

	// The field this issue exists for.
	require.Equal(t, "claude-cli/1.2.3", event["user_agent"])
	require.Equal(t, "203.0.113.7", event["ip_address"])

	for key, want := range map[string]any{
		"user_id":                  float64(11),
		"api_key_id":               float64(22),
		"account_id":               float64(33),
		"group_id":                 float64(44),
		"subscription_id":          float64(55),
		"channel_id":               float64(7),
		"model":                    "gpt-5",
		"requested_model":          "gpt-5-mini",
		"upstream_model":           "gpt-5",
		"model_mapping_chain":      "gpt-5-mini→gpt-5",
		"input_tokens":             float64(100),
		"output_tokens":            float64(200),
		"cache_creation_tokens":    float64(300),
		"cache_read_tokens":        float64(400),
		"cache_creation_5m_tokens": float64(10),
		"cache_creation_1h_tokens": float64(20),
		"image_input_tokens":       float64(5),
		"image_output_tokens":      float64(6),
		"total_cost":               4.75,
		"actual_cost":              4.5,
		"rate_multiplier":          1.2,
		"account_rate_multiplier":  0.8,
		"account_stats_cost":       3.8,
		"billing_type":             float64(1),
		"billing_tier":             "standard",
		"billing_mode":             "token",
		"request_type":             float64(int16(RequestTypeStream)),
		"stream":                   true,
		"service_tier":             "priority",
		"reasoning_effort":         "high",
		"inbound_endpoint":         "/v1/chat/completions",
		"upstream_endpoint":        "/v1/responses",
		"duration_ms":              float64(1200),
		"first_token_ms":           float64(450),
		"session_id":               "sess-xyz",
		"image_count":              float64(2),
		"image_size":               "1024x1024",
		"video_count":              float64(1),
		"video_resolution":         "720p",
		"video_duration_seconds":   float64(8),
	} {
		require.Equal(t, want, event[key], "field %q", key)
	}
}

// Guards against a new usage_logs column silently reaching OpenObserve because
// someone added it to the struct. Update this list deliberately, never blindly.
func TestUsageBusinessEventSchemaSnapshot(t *testing.T) {
	event, _ := captureUsageEvent(t, fullUsageLog(), correlatedUsageContext())

	got := make([]string, 0, len(event))
	for key := range event {
		got = append(got, key)
	}
	sort.Strings(got)

	want := []string{
		// zap encoder keys
		"level", "msg", "service", "time",
		// envelope
		"client_request_id", "event_emitted_at", "event_kind",
		"event_schema_version", "ops_system_log_skip", "request_id", "trace_id",
		// db record identity
		"db_created_at", "db_id", "usage_request_id",
		// payload
		"account_id", "account_rate_multiplier", "account_stats_cost",
		"actual_cost", "api_key_id", "billing_mode", "billing_tier",
		"billing_type", "cache_creation_1h_tokens", "cache_creation_5m_tokens",
		"cache_creation_cost", "cache_creation_tokens", "cache_read_cost",
		"cache_read_tokens", "cache_ttl_overridden", "channel_id",
		"duration_ms", "first_token_ms", "group_id", "image_count",
		"image_input_cost", "image_input_size", "image_input_tokens",
		"image_output_cost", "image_output_size", "image_output_tokens",
		"image_size", "image_size_source", "inbound_endpoint", "input_cost",
		"input_tokens", "ip_address", "long_context_billing_applied", "model",
		"model_mapping_chain", "output_cost", "output_tokens",
		"rate_multiplier", "reasoning_effort", "request_type",
		"requested_model", "service_tier", "session_id", "stream",
		"subscription_id", "total_cost", "upstream_endpoint", "upstream_model",
		"user_agent", "user_id", "video_count", "video_duration_seconds",
		"video_resolution",
	}
	sort.Strings(want)

	require.Equal(t, want, got)
}

func TestUsageBusinessEventHasNoDuplicateKeys(t *testing.T) {
	_, line := captureUsageEvent(t, fullUsageLog(), correlatedUsageContext())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(line), &raw))
	for key := range raw {
		require.Equal(t, 1, strings.Count(line, `"`+key+`":`), "duplicate JSON key %q", key)
	}
}

func TestUsageBusinessEventOmitsEmptyOptionalFields(t *testing.T) {
	// A minimal background settlement row: no inbound request, no media.
	log := &UsageLog{
		UserID:      11,
		APIKeyID:    22,
		AccountID:   33,
		RequestID:   "local:settle-1",
		Model:       "gpt-5",
		InputTokens: 10,
		CreatedAt:   time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC),
	}

	event, _ := captureUsageEvent(t, log, context.Background())

	for _, key := range []string{
		"trace_id", "request_id", "client_request_id", "db_id",
		"user_agent", "ip_address", "session_id", "group_id", "subscription_id",
		"channel_id", "upstream_model", "model_mapping_chain", "billing_tier",
		"billing_mode", "service_tier", "reasoning_effort", "inbound_endpoint",
		"upstream_endpoint", "duration_ms", "first_token_ms",
		"account_rate_multiplier", "account_stats_cost",
		"image_size", "image_input_size", "image_output_size",
		"image_size_source", "video_resolution", "video_duration_seconds",
		"cache_ttl_overridden", "long_context_billing_applied", "stream",
	} {
		require.NotContains(t, event, key, "empty %q must be omitted, not emitted as a zero value", key)
	}

	require.Equal(t, "local:settle-1", event["usage_request_id"])
	require.Equal(t, "gpt-5", event["model"])
	require.Equal(t, float64(10), event["input_tokens"])
}

// RequestedModel is stored falling back to Model, so the projection must match
// what the row actually holds rather than emitting an empty string.
func TestUsageBusinessEventRequestedModelFallsBackToModel(t *testing.T) {
	log := fullUsageLog()
	log.RequestedModel = ""

	event, _ := captureUsageEvent(t, log, correlatedUsageContext())

	require.Equal(t, "gpt-5", event["requested_model"])
}

func TestUsageBusinessEventSkippedWhenDisabled(t *testing.T) {
	buf := &bytes.Buffer{}
	restore := logger.SetBusinessEventWriterForTest(buf)
	t.Cleanup(restore)
	logger.SetBusinessEventsEnabled(false)

	emitUsageBusinessEvent(correlatedUsageContext(), fullUsageLog())

	require.Empty(t, buf.String())
}

func TestUsageBusinessEventNilLogIsNoop(t *testing.T) {
	buf := &bytes.Buffer{}
	restore := logger.SetBusinessEventWriterForTest(buf)
	t.Cleanup(restore)

	emitUsageBusinessEvent(correlatedUsageContext(), nil)

	require.Empty(t, buf.String())
}

// businessEventUsageRepo satisfies just enough of UsageLogRepository for the
// best-effort write funnel; unimplemented methods panic if ever reached.
type businessEventUsageRepo struct {
	UsageLogRepository
	created int
}

func (r *businessEventUsageRepo) Create(context.Context, *UsageLog) (bool, error) {
	r.created++
	return true, nil
}

// Covers the wiring, not just the encoder: every gateway usage write funnels
// through writeUsageLogBestEffort, so the projection must fire there and must
// pick up the caller's correlation IDs.
func TestWriteUsageLogBestEffortEmitsCorrelatedEvent(t *testing.T) {
	buf := &bytes.Buffer{}
	restore := logger.SetBusinessEventWriterForTest(buf)
	t.Cleanup(restore)

	repo := &businessEventUsageRepo{}
	writeUsageLogBestEffort(correlatedUsageContext(), repo, fullUsageLog(), "service.test")

	line := strings.TrimSpace(buf.String())
	require.NotContains(t, line, "\n", "expected exactly one event line")

	var event map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &event))
	require.Equal(t, logger.BusinessEventKindUsageLog, event["event_kind"])
	require.Equal(t, "trace-abc", event["trace_id"])
	require.Equal(t, "req-def", event["request_id"])
	require.Equal(t, "client:abc-123", event["usage_request_id"])
	require.Equal(t, 1, repo.created, "the DB write must still happen")
}

// A disabled switch must not change the write path at all.
func TestWriteUsageLogBestEffortStillWritesWhenEventsDisabled(t *testing.T) {
	buf := &bytes.Buffer{}
	restore := logger.SetBusinessEventWriterForTest(buf)
	t.Cleanup(restore)
	logger.SetBusinessEventsEnabled(false)

	repo := &businessEventUsageRepo{}
	writeUsageLogBestEffort(correlatedUsageContext(), repo, fullUsageLog(), "service.test")

	require.Empty(t, buf.String())
	require.Equal(t, 1, repo.created)
}
