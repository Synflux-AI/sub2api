package logger

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Business events are structured projections of records that are already
// persisted to Postgres. They exist purely so the stdout -> Vector -> OpenObserve
// pipeline can index dimensions (user_agent, model, account_id, ...) that the
// database keeps but the log stream never carried.
//
// The projection is best-effort observability, not a transactional replica:
// Postgres stays the only source of truth. Events are emitted when the record is
// handed to its write path, so OO can hold an event whose row was later dropped
// by a conflict or a full queue. Never use these streams for reconciliation or
// billing aggregation — dedupe on the emitted id fields when counting.
const (
	// BusinessEventKindUsageLog projects usage_logs rows.
	BusinessEventKindUsageLog = "usage_log"
	// BusinessEventKindErrorLog projects ops_error_logs rows.
	BusinessEventKindErrorLog = "error_log"

	// businessEventSchemaVersion is bumped when the envelope changes shape.
	businessEventSchemaVersion = 1

	// businessEventKindKey and friends are the envelope keys. Payload fields
	// colliding with them are dropped so a single event can never contain a
	// duplicate JSON key.
	businessEventKindKey        = "event_kind"
	businessEventSchemaKey      = "event_schema_version"
	businessEventEmittedAtKey   = "event_emitted_at"
	businessEventTraceIDKey     = "trace_id"
	businessEventRequestIDKey   = "request_id"
	businessEventClientReqIDKey = "client_request_id"
)

var (
	businessEventsEnabled atomic.Bool
	businessEventLogger   atomic.Pointer[zap.Logger]
	businessEventOnce     sync.Once
)

// SetBusinessEventsEnabled toggles emission at runtime. Defaults to off so a
// deploy can roll out the code and enable it node by node.
func SetBusinessEventsEnabled(enabled bool) {
	businessEventsEnabled.Store(enabled)
}

// BusinessEventsEnabled reports whether business events are currently emitted.
func BusinessEventsEnabled() bool {
	return businessEventsEnabled.Load()
}

// SetBusinessEventWriterForTest redirects business events to w, enables
// emission, and returns a func restoring both. Intended for tests in packages
// that build event payloads and need to assert on the encoded line.
func SetBusinessEventWriterForTest(w io.Writer) func() {
	prevLogger := businessEventLogger.Load()
	prevEnabled := businessEventsEnabled.Load()

	businessEventLogger.Store(buildBusinessEventLogger("sub2api-test", zapcore.AddSync(w)))
	businessEventsEnabled.Store(true)

	return func() {
		businessEventLogger.Store(prevLogger)
		businessEventsEnabled.Store(prevEnabled)
	}
}

// businessEventWriteSyncer returns the destination for business events. It is
// always the shared locked stdout syncer — the same one the main logger writes
// through — because Vector reads container stdout, and because two independent
// zapcore.Lock wrappers over the same fd would not serialize against each
// other: partial writes could interleave and Docker's json-file driver would
// hand Vector a broken JSON line.
func businessEventWriteSyncer() zapcore.WriteSyncer {
	return sharedStdoutSyncer
}

func businessEventEncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		MessageKey:     "msg",
		NameKey:        zapcore.OmitKey,
		CallerKey:      zapcore.OmitKey,
		FunctionKey:    zapcore.OmitKey,
		StacktraceKey:  zapcore.OmitKey,
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.RFC3339NanoTimeEncoder,
		EncodeDuration: zapcore.MillisDurationEncoder,
	}
}

// buildBusinessEventLogger constructs a logger that is deliberately independent
// of the main logger's configuration:
//
//   - always-enabled level, so log.level=warn does not suppress events;
//   - no sampling wrapper, so a high-volume message is never deduplicated away;
//   - no sinkCore wrapper, so events cannot reach OpsSystemLogSink and loop back
//     into the database;
//   - JSON only, regardless of log.format, since Vector parses JSON.
func buildBusinessEventLogger(serviceName string, ws zapcore.WriteSyncer) *zap.Logger {
	alwaysOn := zap.LevelEnablerFunc(func(zapcore.Level) bool { return true })
	core := zapcore.NewCore(zapcore.NewJSONEncoder(businessEventEncoderConfig()), ws, alwaysOn)
	return zap.New(core).With(zap.String("service", serviceName))
}

// configureBusinessEventLogger rebuilds the emitter so it picks up the current
// service name. Called from initLocked while the logger mutex is held.
func configureBusinessEventLogger(serviceName string) {
	businessEventLogger.Store(buildBusinessEventLogger(serviceName, businessEventWriteSyncer()))
}

func businessEventLoggerOrInit() *zap.Logger {
	if l := businessEventLogger.Load(); l != nil {
		return l
	}
	// Emission before logger.Init (or in a test binary that never calls it)
	// still needs a working writer rather than a silent drop.
	businessEventOnce.Do(func() {
		if businessEventLogger.Load() == nil {
			configureBusinessEventLogger("sub2api")
		}
	})
	return businessEventLogger.Load()
}

// EmitBusinessEvent writes one structured business event to stdout.
//
// The write is synchronous: it adds no queue, no drop counter and no shutdown
// flush, because it is the same class of stdout write the request logger
// already performs for every request. Correlation IDs are read from ctx, so
// callers must emit while the request context is still in hand rather than from
// a detached background task.
//
// Payload fields must be an explicit whitelist built by the caller. Never
// reflect a whole database struct into this function: new columns would then
// leak to OpenObserve automatically.
func EmitBusinessEvent(ctx context.Context, kind string, fields ...zap.Field) {
	if !businessEventsEnabled.Load() {
		return
	}
	l := businessEventLoggerOrInit()
	if l == nil {
		return
	}

	envelope := make([]zap.Field, 0, len(fields)+7)
	envelope = append(envelope,
		zap.String(businessEventKindKey, kind),
		zap.Int(businessEventSchemaKey, businessEventSchemaVersion),
		zap.Time(businessEventEmittedAtKey, time.Now().UTC()),
		zap.Bool(OpsSystemLogSkipField, true),
	)
	envelope = appendCorrelationFields(envelope, ctx)

	l.Info(kind, dedupeFields(envelope, fields)...)
}

// appendCorrelationFields copies the request-scoped correlation IDs out of ctx.
// Absent IDs are omitted rather than emitted as empty strings so OO keeps a
// stable type per field. Background writers (live settlement, batch image) have
// no inbound trace and legitimately emit none of these.
func appendCorrelationFields(dst []zap.Field, ctx context.Context) []zap.Field {
	if ctx == nil {
		return dst
	}
	for _, pair := range []struct {
		key    string
		ctxKey ctxkey.Key
	}{
		{businessEventTraceIDKey, ctxkey.TraceID},
		{businessEventRequestIDKey, ctxkey.RequestID},
		{businessEventClientReqIDKey, ctxkey.ClientRequestID},
	} {
		value, _ := ctx.Value(pair.ctxKey).(string)
		if value = strings.TrimSpace(value); value != "" {
			dst = append(dst, zap.String(pair.key, value))
		}
	}
	return dst
}

// dedupeFields appends payload onto envelope, skipping any payload field whose
// key the envelope already owns. zap's JSON encoder writes duplicate keys
// verbatim, and a duplicated key makes the event ambiguous for every consumer;
// the envelope is authoritative, so it wins.
func dedupeFields(envelope, payload []zap.Field) []zap.Field {
	if len(payload) == 0 {
		return envelope
	}
	seen := make(map[string]struct{}, len(envelope)+len(payload))
	for _, field := range envelope {
		seen[field.Key] = struct{}{}
	}
	out := envelope
	for _, field := range payload {
		if _, exists := seen[field.Key]; exists {
			continue
		}
		seen[field.Key] = struct{}{}
		out = append(out, field)
	}
	return out
}
