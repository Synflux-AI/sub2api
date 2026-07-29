package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// captureBusinessEvents redirects the business-event writer to a buffer for the
// duration of the test and enables emission.
func captureBusinessEvents(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	prevLogger := businessEventLogger.Load()
	prevEnabled := businessEventsEnabled.Load()

	businessEventLogger.Store(buildBusinessEventLogger("sub2api-test", zapcore.AddSync(buf)))
	businessEventsEnabled.Store(true)

	t.Cleanup(func() {
		businessEventLogger.Store(prevLogger)
		businessEventsEnabled.Store(prevEnabled)
	})
	return buf
}

func decodeSingleEvent(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := splitNonEmptyLines(buf.String())
	require.Len(t, lines, 1, "expected exactly one business event line, got: %q", buf.String())

	var event map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &event))
	return event
}

func splitNonEmptyLines(s string) []string {
	out := make([]string, 0, 2)
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func correlatedContext() context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-abc")
	ctx = context.WithValue(ctx, ctxkey.RequestID, "req-def")
	return context.WithValue(ctx, ctxkey.ClientRequestID, "client-ghi")
}

func TestEmitBusinessEventDisabledByDefault(t *testing.T) {
	buf := &bytes.Buffer{}
	prevLogger := businessEventLogger.Load()
	prevEnabled := businessEventsEnabled.Load()
	businessEventLogger.Store(buildBusinessEventLogger("sub2api-test", zapcore.AddSync(buf)))
	businessEventsEnabled.Store(false)
	t.Cleanup(func() {
		businessEventLogger.Store(prevLogger)
		businessEventsEnabled.Store(prevEnabled)
	})

	EmitBusinessEvent(correlatedContext(), BusinessEventKindErrorLog, zap.String("model", "gpt-5"))

	require.Empty(t, buf.String(), "business events must not be emitted while the switch is off")
}

func TestEmitBusinessEventEnvelope(t *testing.T) {
	buf := captureBusinessEvents(t)

	EmitBusinessEvent(correlatedContext(), BusinessEventKindErrorLog, zap.String("model", "gpt-5"))

	event := decodeSingleEvent(t, buf)
	require.Equal(t, BusinessEventKindErrorLog, event["event_kind"])
	require.EqualValues(t, businessEventSchemaVersion, event["event_schema_version"])
	require.Equal(t, "trace-abc", event["trace_id"])
	require.Equal(t, "req-def", event["request_id"])
	require.Equal(t, "client-ghi", event["client_request_id"])
	require.Equal(t, true, event[OpsSystemLogSkipField])
	require.Equal(t, "gpt-5", event["model"])
	require.NotEmpty(t, event["event_emitted_at"])
	require.Equal(t, "sub2api-test", event["service"])
}

func TestEmitBusinessEventOmitsMissingCorrelationIDs(t *testing.T) {
	buf := captureBusinessEvents(t)

	// Only trace_id present: request_id / client_request_id must be absent
	// rather than emitted as empty strings, so OO keeps stable field types.
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-only")
	EmitBusinessEvent(ctx, BusinessEventKindErrorLog)

	event := decodeSingleEvent(t, buf)
	require.Equal(t, "trace-only", event["trace_id"])
	require.NotContains(t, event, "request_id")
	require.NotContains(t, event, "client_request_id")
}

func TestEmitBusinessEventDropsFieldsCollidingWithEnvelope(t *testing.T) {
	buf := captureBusinessEvents(t)

	EmitBusinessEvent(
		correlatedContext(),
		BusinessEventKindUsageLog,
		zap.String("event_kind", "spoofed"),
		zap.String("trace_id", "spoofed"),
		zap.Bool(OpsSystemLogSkipField, false),
		zap.String("model", "gpt-5"),
	)

	line := splitNonEmptyLines(buf.String())
	require.Len(t, line, 1)

	// The envelope must win, and the raw line must not carry duplicate JSON keys
	// (encoding/json silently keeps the last one, so assert on the raw bytes).
	require.Equal(t, 1, strings.Count(line[0], `"event_kind":`))
	require.Equal(t, 1, strings.Count(line[0], `"trace_id":`))
	require.Equal(t, 1, strings.Count(line[0], `"`+OpsSystemLogSkipField+`":`))

	event := decodeSingleEvent(t, buf)
	require.Equal(t, BusinessEventKindUsageLog, event["event_kind"])
	require.Equal(t, "trace-abc", event["trace_id"])
	require.Equal(t, true, event[OpsSystemLogSkipField])
	require.Equal(t, "gpt-5", event["model"])
}

// The whole point of a dedicated emitter: business events must survive a
// production logger configured to warn level with sampling turned on.
func TestEmitBusinessEventIgnoresGlobalLevelAndSampling(t *testing.T) {
	require.NoError(t, Init(InitOptions{
		Level:  "warn",
		Format: "json",
		Output: OutputOptions{ToStdout: true},
		Sampling: SamplingOptions{
			Enabled:    true,
			Initial:    1,
			Thereafter: 1000000,
		},
	}))

	buf := captureBusinessEvents(t)

	const emitCount = 5
	for i := 0; i < emitCount; i++ {
		EmitBusinessEvent(correlatedContext(), BusinessEventKindUsageLog, zap.Int("seq", i))
	}

	require.Len(t, splitNonEmptyLines(buf.String()), emitCount,
		"every business event must be written regardless of log.level and log.sampling")
}

type recordingSink struct {
	mu     sync.Mutex
	events []*LogEvent
}

func (s *recordingSink) WriteLogEvent(event *LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// Business events must never reach the database-backed Ops system-log sink.
func TestEmitBusinessEventBypassesSink(t *testing.T) {
	require.NoError(t, Init(InitOptions{
		Level:  "info",
		Format: "json",
		Output: OutputOptions{ToStdout: true},
	}))

	sink := &recordingSink{}
	SetSink(sink)
	t.Cleanup(func() { SetSink(nil) })

	buf := captureBusinessEvents(t)

	EmitBusinessEvent(correlatedContext(), BusinessEventKindErrorLog, zap.String("model", "gpt-5"))

	require.Len(t, splitNonEmptyLines(buf.String()), 1)
	require.Zero(t, sink.count(), "business events must not be indexed into ops_system_logs")
}

// Interleaved partial writes on the shared stdout fd would make Docker's
// json-file driver hand Vector broken JSON, so both writers must serialize on
// the same lock.
func TestBusinessEventsShareStdoutSyncerWithMainLogger(t *testing.T) {
	require.Same(t, sharedStdoutSyncer, businessEventWriteSyncer(),
		"business events must reuse the locked stdout syncer used by the main logger")
}
