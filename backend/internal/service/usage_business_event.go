package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// emitUsageBusinessEvent projects one usage_logs record to stdout for the
// Vector -> OpenObserve pipeline.
//
// The field list is an explicit whitelist mirroring the 57 usage_logs columns
// written by prepareUsageLogInsert, deliberately not reflection over UsageLog:
// a new column must not leak to OpenObserve just because someone added it to
// the struct. TestUsageBusinessEventSchemaSnapshot fails when this drifts.
//
// Two struct fields are intentionally absent:
//
//   - MediaType, which is not a usage_logs column at all;
//   - ImageSizeBreakdown, a map of per-size counts. It is a detail blob rather
//     than a search dimension, and emitting a nested object would give the
//     stream a field whose shape varies per event.
//
// The record's own relations (User / APIKey / Account / Group / Subscription)
// are never walked: they would drag emails and key material into the stream.
func emitUsageBusinessEvent(ctx context.Context, log *UsageLog) {
	if log == nil || !logger.BusinessEventsEnabled() {
		return
	}

	fields := make([]zap.Field, 0, 56)
	appendIf := func(field zap.Field, keep bool) {
		if keep {
			fields = append(fields, field)
		}
	}

	// Record identity. db_id is only present once the row has an ID; on the
	// best-effort write path the projection runs before the insert, so it is
	// usually absent rather than faked.
	appendIf(zap.Int64("db_id", log.ID), log.ID != 0)
	if !log.CreatedAt.IsZero() {
		fields = append(fields, zap.Time("db_created_at", log.CreatedAt.UTC()))
	}
	// usage_logs.request_id is a billing/idempotency key ("client:..." /
	// "local:..."), not the request-scoped id. The envelope owns request_id, so
	// this one is emitted under its own name.
	usageAppendNonEmpty(&fields, "usage_request_id", strings.TrimSpace(log.RequestID))

	// Identity
	appendIf(zap.Int64("user_id", log.UserID), log.UserID != 0)
	appendIf(zap.Int64("api_key_id", log.APIKeyID), log.APIKeyID != 0)
	appendIf(zap.Int64("account_id", log.AccountID), log.AccountID != 0)
	usageAppendInt64Ptr(&fields, "group_id", log.GroupID)
	usageAppendInt64Ptr(&fields, "subscription_id", log.SubscriptionID)
	usageAppendInt64Ptr(&fields, "channel_id", log.ChannelID)

	// Model routing. requested_model mirrors the insert's fallback to Model, so
	// the event matches the row rather than reporting an empty string.
	usageAppendNonEmpty(&fields, "model", log.Model)
	requestedModel := strings.TrimSpace(log.RequestedModel)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(log.Model)
	}
	usageAppendNonEmpty(&fields, "requested_model", requestedModel)
	usageAppendStringPtr(&fields, "upstream_model", log.UpstreamModel)
	usageAppendStringPtr(&fields, "model_mapping_chain", log.ModelMappingChain)

	// Tokens
	appendIf(zap.Int("input_tokens", log.InputTokens), log.InputTokens != 0)
	appendIf(zap.Int("output_tokens", log.OutputTokens), log.OutputTokens != 0)
	appendIf(zap.Int("cache_creation_tokens", log.CacheCreationTokens), log.CacheCreationTokens != 0)
	appendIf(zap.Int("cache_read_tokens", log.CacheReadTokens), log.CacheReadTokens != 0)
	appendIf(zap.Int("cache_creation_5m_tokens", log.CacheCreation5mTokens), log.CacheCreation5mTokens != 0)
	appendIf(zap.Int("cache_creation_1h_tokens", log.CacheCreation1hTokens), log.CacheCreation1hTokens != 0)
	appendIf(zap.Int("image_input_tokens", log.ImageInputTokens), log.ImageInputTokens != 0)
	appendIf(zap.Int("image_output_tokens", log.ImageOutputTokens), log.ImageOutputTokens != 0)

	// Cost
	appendIf(zap.Float64("input_cost", log.InputCost), log.InputCost != 0)
	appendIf(zap.Float64("output_cost", log.OutputCost), log.OutputCost != 0)
	appendIf(zap.Float64("cache_creation_cost", log.CacheCreationCost), log.CacheCreationCost != 0)
	appendIf(zap.Float64("cache_read_cost", log.CacheReadCost), log.CacheReadCost != 0)
	appendIf(zap.Float64("image_input_cost", log.ImageInputCost), log.ImageInputCost != 0)
	appendIf(zap.Float64("image_output_cost", log.ImageOutputCost), log.ImageOutputCost != 0)
	appendIf(zap.Float64("total_cost", log.TotalCost), log.TotalCost != 0)
	appendIf(zap.Float64("actual_cost", log.ActualCost), log.ActualCost != 0)
	appendIf(zap.Float64("rate_multiplier", log.RateMultiplier), log.RateMultiplier != 0)
	usageAppendFloat64Ptr(&fields, "account_rate_multiplier", log.AccountRateMultiplier)
	usageAppendFloat64Ptr(&fields, "account_stats_cost", log.AccountStatsCost)

	// Billing shape
	appendIf(zap.Int8("billing_type", log.BillingType), log.BillingType != 0)
	usageAppendStringPtr(&fields, "billing_tier", log.BillingTier)
	usageAppendStringPtr(&fields, "billing_mode", log.BillingMode)
	appendIf(zap.Bool("cache_ttl_overridden", log.CacheTTLOverridden), log.CacheTTLOverridden)
	appendIf(zap.Bool("long_context_billing_applied", log.LongContextBillingApplied), log.LongContextBillingApplied)

	// Request shape. request_type is the normalized enum; stream / openai_ws_mode
	// are its legacy projections and are only emitted when actually set.
	requestType := log.EffectiveRequestType()
	appendIf(zap.Int16("request_type", int16(requestType)), requestType != RequestTypeUnknown)
	appendIf(zap.Bool("stream", log.Stream), log.Stream)
	appendIf(zap.Bool("openai_ws_mode", log.OpenAIWSMode), log.OpenAIWSMode)
	usageAppendStringPtr(&fields, "service_tier", log.ServiceTier)
	usageAppendStringPtr(&fields, "reasoning_effort", log.ReasoningEffort)
	usageAppendStringPtr(&fields, "inbound_endpoint", log.InboundEndpoint)
	usageAppendStringPtr(&fields, "upstream_endpoint", log.UpstreamEndpoint)

	// Client identity
	usageAppendStringPtr(&fields, "user_agent", log.UserAgent)
	usageAppendStringPtr(&fields, "ip_address", log.IPAddress)
	usageAppendStringPtr(&fields, "session_id", log.SessionID)

	// Media
	appendIf(zap.Int("image_count", log.ImageCount), log.ImageCount != 0)
	usageAppendStringPtr(&fields, "image_size", log.ImageSize)
	usageAppendStringPtr(&fields, "image_input_size", log.ImageInputSize)
	usageAppendStringPtr(&fields, "image_output_size", log.ImageOutputSize)
	usageAppendStringPtr(&fields, "image_size_source", log.ImageSizeSource)
	appendIf(zap.Int("video_count", log.VideoCount), log.VideoCount != 0)
	usageAppendStringPtr(&fields, "video_resolution", log.VideoResolution)
	usageAppendIntPtr(&fields, "video_duration_seconds", log.VideoDurationSeconds)

	// Latency
	usageAppendIntPtr(&fields, "duration_ms", log.DurationMs)
	usageAppendIntPtr(&fields, "first_token_ms", log.FirstTokenMs)

	logger.EmitBusinessEvent(ctx, logger.BusinessEventKindUsageLog, fields...)
}

func usageAppendNonEmpty(fields *[]zap.Field, key, value string) {
	if value != "" {
		*fields = append(*fields, zap.String(key, value))
	}
}

func usageAppendStringPtr(fields *[]zap.Field, key string, value *string) {
	if value != nil && strings.TrimSpace(*value) != "" {
		*fields = append(*fields, zap.String(key, *value))
	}
}

func usageAppendInt64Ptr(fields *[]zap.Field, key string, value *int64) {
	if value != nil {
		*fields = append(*fields, zap.Int64(key, *value))
	}
}

func usageAppendIntPtr(fields *[]zap.Field, key string, value *int) {
	if value != nil {
		*fields = append(*fields, zap.Int(key, *value))
	}
}

func usageAppendFloat64Ptr(fields *[]zap.Field, key string, value *float64) {
	if value != nil {
		*fields = append(*fields, zap.Float64(key, *value))
	}
}
