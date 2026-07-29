package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// opsErrorLogRequestContext returns the context carrying the correlation IDs.
// They live on c.Request's context (middleware.RequestLogger replaces the
// request with one holding trace_id / request_id / client_request_id), not on
// the gin.Context itself.
func opsErrorLogRequestContext(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}

// opsBusinessEventMaxErrorMessageBytes bounds the high-level error message. The
// message is a classification summary, not response content, but it is still
// caller-influenced, so it gets an explicit ceiling.
const opsBusinessEventMaxErrorMessageBytes = 1024

// emitOpsErrorBusinessEvent projects one ops_error_logs entry to stdout for the
// Vector -> OpenObserve pipeline.
//
// The field list below is an explicit whitelist, deliberately not reflection
// over service.OpsInsertErrorLogInput: a new database column must not leak to
// OpenObserve just because someone added it to the struct. Adding, renaming or
// removing a field here fails TestOpsErrorBusinessEventSchemaIsLocked, which
// compares the emitted key set against a golden list.
//
// Never projected, by design:
//
//   - ErrorBody
//   - UpstreamErrorMessage / UpstreamErrorDetail
//   - UpstreamErrors / UpstreamErrorsJSON (carry upstream_response_body)
//
// service.SanitizeOpsErrorBodyForQueue removes credentials, not prompts, user
// input or PII, so "already sanitized" is not grounds for copying payloads into
// a second store with its own retention and access rules. If those bodies are
// ever needed in OpenObserve, that needs a separate stream with shorter
// retention and its own switch.
//
// API keys appear only as APIKeyID and the existing 8-char masked prefix; the
// plaintext key is never emitted.
func emitOpsErrorBusinessEvent(ctx context.Context, entry *service.OpsInsertErrorLogInput) {
	if entry == nil || !logger.BusinessEventsEnabled() {
		return
	}

	// Nullable fields are omitted when empty rather than emitted as zero values,
	// so each field keeps one stable type across every event in the stream.
	fields := make([]zap.Field, 0, 32)
	appendField := func(field zap.Field, keep bool) {
		if keep {
			fields = append(fields, field)
		}
	}

	if !entry.CreatedAt.IsZero() {
		fields = append(fields, zap.Time("db_created_at", entry.CreatedAt.UTC()))
	}

	// Identity
	appendInt64Ptr(&fields, "user_id", entry.UserID)
	appendInt64Ptr(&fields, "api_key_id", entry.APIKeyID)
	appendInt64Ptr(&fields, "account_id", entry.AccountID)
	appendInt64Ptr(&fields, "group_id", entry.GroupID)
	appendNonEmpty(&fields, "api_key_prefix", entry.APIKeyPrefix)
	appendStringPtr(&fields, "client_ip", entry.ClientIP)

	// Request shape
	appendNonEmpty(&fields, "platform", entry.Platform)
	appendNonEmpty(&fields, "model", entry.Model)
	appendNonEmpty(&fields, "requested_model", entry.RequestedModel)
	appendNonEmpty(&fields, "upstream_model", entry.UpstreamModel)
	appendNonEmpty(&fields, "request_path", entry.RequestPath)
	appendNonEmpty(&fields, "inbound_endpoint", entry.InboundEndpoint)
	appendNonEmpty(&fields, "upstream_endpoint", entry.UpstreamEndpoint)
	if entry.RequestType != nil {
		fields = append(fields, zap.Int16("request_type", *entry.RequestType))
	}
	appendField(zap.Bool("stream", entry.Stream), entry.Stream)
	// UA reuses the entry-point normalization: valid UTF-8, 512-byte ceiling.
	// enqueueOpsErrorLog already normalized entry.UserAgent in place; the call is
	// idempotent and repeated here so the projection's own bound does not depend
	// on which caller reached it first.
	appendNonEmpty(&fields, "user_agent", normalizeOpsPersistentUserAgent(entry.UserAgent))

	// Classification
	appendNonEmpty(&fields, "error_phase", entry.ErrorPhase)
	appendNonEmpty(&fields, "error_type", entry.ErrorType)
	appendNonEmpty(&fields, "severity", entry.Severity)
	appendField(zap.Int("status_code", entry.StatusCode), entry.StatusCode != 0)
	if entry.UpstreamStatusCode != nil {
		fields = append(fields, zap.Int("upstream_status_code", *entry.UpstreamStatusCode))
	}
	appendField(zap.Bool("is_business_limited", entry.IsBusinessLimited), entry.IsBusinessLimited)
	appendField(zap.Bool("is_count_tokens", entry.IsCountTokens), entry.IsCountTokens)
	appendNonEmpty(&fields, "error_source", entry.ErrorSource)
	appendNonEmpty(&fields, "error_owner", entry.ErrorOwner)
	appendNonEmpty(&fields, "error_message", truncateString(entry.ErrorMessage, opsBusinessEventMaxErrorMessageBytes))

	// Latency breakdown
	appendInt64Ptr(&fields, "auth_latency_ms", entry.AuthLatencyMs)
	appendInt64Ptr(&fields, "routing_latency_ms", entry.RoutingLatencyMs)
	appendInt64Ptr(&fields, "upstream_latency_ms", entry.UpstreamLatencyMs)
	appendInt64Ptr(&fields, "response_latency_ms", entry.ResponseLatencyMs)
	appendInt64Ptr(&fields, "time_to_first_token_ms", entry.TimeToFirstTokenMs)

	// entry.RequestID / entry.ClientRequestID are intentionally absent: the
	// handler reads both straight off the request context, so the envelope's
	// request_id / client_request_id already carry the same values. Emitting
	// them again would duplicate keys. (usage_logs.request_id is different — it
	// is a billing/idempotency key, and the usage projection renames it to
	// usage_request_id.)
	logger.EmitBusinessEvent(ctx, logger.BusinessEventKindErrorLog, fields...)
}

func appendNonEmpty(fields *[]zap.Field, key, value string) {
	if value != "" {
		*fields = append(*fields, zap.String(key, value))
	}
}

func appendStringPtr(fields *[]zap.Field, key string, value *string) {
	if value != nil && *value != "" {
		*fields = append(*fields, zap.String(key, *value))
	}
}

func appendInt64Ptr(fields *[]zap.Field, key string, value *int64) {
	if value != nil {
		*fields = append(*fields, zap.Int64(key, *value))
	}
}
