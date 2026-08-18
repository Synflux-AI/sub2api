package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

var (
	dashboardTrendCache        = newSnapshotCache(30 * time.Second)
	dashboardModelStatsCache   = newSnapshotCache(30 * time.Second)
	dashboardGroupStatsCache   = newSnapshotCache(30 * time.Second)
	dashboardUsersTrendCache   = newSnapshotCache(30 * time.Second)
	dashboardAPIKeysTrendCache = newSnapshotCache(30 * time.Second)
)

type dashboardTrendCacheKey struct {
	StartTime             string   `json:"start_time"`
	EndTime               string   `json:"end_time"`
	Granularity           string   `json:"granularity"`
	UserID                int64    `json:"user_id"`
	UserIDs               []int64  `json:"user_ids,omitempty"`
	APIKeyID              int64    `json:"api_key_id"`
	AccountID             int64    `json:"account_id"`
	AccountIDs            []int64  `json:"account_ids,omitempty"`
	GroupID               int64    `json:"group_id"`
	GroupIDs              []int64  `json:"group_ids,omitempty"`
	Model                 string   `json:"model"`
	Models                []string `json:"models,omitempty"`
	RequestType           *int16   `json:"request_type"`
	Stream                *bool    `json:"stream"`
	BillingType           *int8    `json:"billing_type"`
	UpstreamModelMismatch *bool    `json:"upstream_model_mismatch"`
	IncludeLatency        bool     `json:"include_latency"`
}

type dashboardModelGroupCacheKey struct {
	StartTime             string `json:"start_time"`
	EndTime               string `json:"end_time"`
	UserID                int64  `json:"user_id"`
	APIKeyID              int64  `json:"api_key_id"`
	AccountID             int64  `json:"account_id"`
	GroupID               int64  `json:"group_id"`
	ModelSource           string `json:"model_source,omitempty"`
	RequestType           *int16 `json:"request_type"`
	Stream                *bool  `json:"stream"`
	BillingType           *int8  `json:"billing_type"`
	UpstreamModelMismatch *bool  `json:"upstream_model_mismatch"`
}

type dashboardEntityTrendCacheKey struct {
	StartTime             string   `json:"start_time"`
	EndTime               string   `json:"end_time"`
	Granularity           string   `json:"granularity"`
	Limit                 int      `json:"limit"`
	SortBy                string   `json:"sort_by,omitempty"`
	UserID                int64    `json:"user_id"`
	UserIDs               []int64  `json:"user_ids,omitempty"`
	APIKeyID              int64    `json:"api_key_id"`
	AccountID             int64    `json:"account_id"`
	AccountIDs            []int64  `json:"account_ids,omitempty"`
	GroupID               int64    `json:"group_id"`
	GroupIDs              []int64  `json:"group_ids,omitempty"`
	Model                 string   `json:"model"`
	Models                []string `json:"models,omitempty"`
	RequestType           *int16   `json:"request_type"`
	Stream                *bool    `json:"stream"`
	BillingType           *int8    `json:"billing_type"`
	UpstreamModelMismatch *bool    `json:"upstream_model_mismatch"`
}

func cacheStatusValue(hit bool) string {
	if hit {
		return "hit"
	}
	return "miss"
}

func mustMarshalDashboardCacheKey(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

func snapshotPayloadAs[T any](payload any) (T, error) {
	typed, ok := payload.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("unexpected cache payload type %T", payload)
	}
	return typed, nil
}

func (h *DashboardHandler) getUsageTrendCached(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	userID, apiKeyID, accountID, groupID int64,
	model string,
	requestType *int16,
	stream *bool,
	billingType *int8,
	upstreamModelMismatch *bool,
	includeLatency bool,
) ([]usagestats.TrendDataPoint, bool, error) {
	return h.getUsageTrendCachedWithFilters(ctx, startTime, endTime, granularity, usagestats.UsageLogFilters{
		UserID: userID, APIKeyID: apiKeyID, AccountID: accountID, GroupID: groupID,
		Model: model, ModelFilterSource: usagestats.ModelSourceRequested,
		RequestType: requestType, Stream: stream, BillingType: billingType,
		UpstreamModelMismatch: upstreamModelMismatch, IncludeLatency: includeLatency,
	})
}

func (h *DashboardHandler) getUsageTrendCachedWithFilters(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	filters usagestats.UsageLogFilters,
) ([]usagestats.TrendDataPoint, bool, error) {
	key := mustMarshalDashboardCacheKey(dashboardTrendCacheKey{
		StartTime:   startTime.UTC().Format(time.RFC3339),
		EndTime:     endTime.UTC().Format(time.RFC3339),
		Granularity: granularity,
		UserID:      filters.UserID, UserIDs: filters.UserIDs,
		APIKeyID:  filters.APIKeyID,
		AccountID: filters.AccountID, AccountIDs: filters.AccountIDs,
		GroupID: filters.GroupID, GroupIDs: filters.GroupIDs,
		Model: filters.Model, Models: filters.Models,
		RequestType: filters.RequestType, Stream: filters.Stream, BillingType: filters.BillingType,
		UpstreamModelMismatch: filters.UpstreamModelMismatch,
		IncludeLatency:        filters.IncludeLatency,
	})
	entry, hit, err := dashboardTrendCache.GetOrLoad(key, func() (any, error) {
		return h.dashboardService.GetUsageTrendWithUsageFilters(ctx, startTime, endTime, granularity, filters)
	})
	if err != nil {
		return nil, hit, err
	}
	trend, err := snapshotPayloadAs[[]usagestats.TrendDataPoint](entry.Payload)
	return trend, hit, err
}

func (h *DashboardHandler) getModelStatsCached(
	ctx context.Context,
	startTime, endTime time.Time,
	userID, apiKeyID, accountID, groupID int64,
	modelSource string,
	requestType *int16,
	stream *bool,
	billingType *int8,
	upstreamModelMismatch *bool,
) ([]usagestats.ModelStat, bool, error) {
	key := mustMarshalDashboardCacheKey(dashboardModelGroupCacheKey{
		StartTime:             startTime.UTC().Format(time.RFC3339),
		EndTime:               endTime.UTC().Format(time.RFC3339),
		UserID:                userID,
		APIKeyID:              apiKeyID,
		AccountID:             accountID,
		GroupID:               groupID,
		ModelSource:           usagestats.NormalizeModelSource(modelSource),
		RequestType:           requestType,
		Stream:                stream,
		BillingType:           billingType,
		UpstreamModelMismatch: upstreamModelMismatch,
	})
	entry, hit, err := dashboardModelStatsCache.GetOrLoad(key, func() (any, error) {
		return h.dashboardService.GetModelStatsWithUsageFiltersBySource(ctx, startTime, endTime, usagestats.UsageLogFilters{
			UserID: userID, APIKeyID: apiKeyID, AccountID: accountID, GroupID: groupID,
			RequestType: requestType, Stream: stream, BillingType: billingType,
			UpstreamModelMismatch: upstreamModelMismatch,
		}, modelSource)
	})
	if err != nil {
		return nil, hit, err
	}
	stats, err := snapshotPayloadAs[[]usagestats.ModelStat](entry.Payload)
	return stats, hit, err
}

func (h *DashboardHandler) getGroupStatsCached(
	ctx context.Context,
	startTime, endTime time.Time,
	userID, apiKeyID, accountID, groupID int64,
	requestType *int16,
	stream *bool,
	billingType *int8,
	upstreamModelMismatch *bool,
) ([]usagestats.GroupStat, bool, error) {
	key := mustMarshalDashboardCacheKey(dashboardModelGroupCacheKey{
		StartTime:             startTime.UTC().Format(time.RFC3339),
		EndTime:               endTime.UTC().Format(time.RFC3339),
		UserID:                userID,
		APIKeyID:              apiKeyID,
		AccountID:             accountID,
		GroupID:               groupID,
		RequestType:           requestType,
		Stream:                stream,
		BillingType:           billingType,
		UpstreamModelMismatch: upstreamModelMismatch,
	})
	entry, hit, err := dashboardGroupStatsCache.GetOrLoad(key, func() (any, error) {
		return h.dashboardService.GetGroupStatsWithUsageFilters(ctx, startTime, endTime, usagestats.UsageLogFilters{
			UserID: userID, APIKeyID: apiKeyID, AccountID: accountID, GroupID: groupID,
			RequestType: requestType, Stream: stream, BillingType: billingType,
			UpstreamModelMismatch: upstreamModelMismatch,
		})
	})
	if err != nil {
		return nil, hit, err
	}
	stats, err := snapshotPayloadAs[[]usagestats.GroupStat](entry.Payload)
	return stats, hit, err
}

func (h *DashboardHandler) getAPIKeyUsageTrendCached(ctx context.Context, startTime, endTime time.Time, granularity string, limit int) ([]usagestats.APIKeyUsageTrendPoint, bool, error) {
	key := mustMarshalDashboardCacheKey(dashboardEntityTrendCacheKey{
		StartTime:   startTime.UTC().Format(time.RFC3339),
		EndTime:     endTime.UTC().Format(time.RFC3339),
		Granularity: granularity,
		Limit:       limit,
	})
	entry, hit, err := dashboardAPIKeysTrendCache.GetOrLoad(key, func() (any, error) {
		return h.dashboardService.GetAPIKeyUsageTrend(ctx, startTime, endTime, granularity, limit)
	})
	if err != nil {
		return nil, hit, err
	}
	trend, err := snapshotPayloadAs[[]usagestats.APIKeyUsageTrendPoint](entry.Payload)
	return trend, hit, err
}

func (h *DashboardHandler) getUserUsageTrendCached(ctx context.Context, startTime, endTime time.Time, granularity string, limit int, sortBy string, filters usagestats.UsageLogFilters) ([]usagestats.UserUsageTrendPoint, bool, error) {
	key := mustMarshalDashboardCacheKey(dashboardEntityTrendCacheKey{
		StartTime:             startTime.UTC().Format(time.RFC3339),
		EndTime:               endTime.UTC().Format(time.RFC3339),
		Granularity:           granularity,
		Limit:                 limit,
		SortBy:                sortBy,
		UserID:                filters.UserID,
		UserIDs:               filters.UserIDs,
		APIKeyID:              filters.APIKeyID,
		AccountID:             filters.AccountID,
		AccountIDs:            filters.AccountIDs,
		GroupID:               filters.GroupID,
		GroupIDs:              filters.GroupIDs,
		Model:                 filters.Model,
		Models:                filters.Models,
		RequestType:           filters.RequestType,
		Stream:                filters.Stream,
		BillingType:           filters.BillingType,
		UpstreamModelMismatch: filters.UpstreamModelMismatch,
	})
	entry, hit, err := dashboardUsersTrendCache.GetOrLoad(key, func() (any, error) {
		return h.dashboardService.GetUserUsageTrend(ctx, startTime, endTime, granularity, limit, sortBy, filters)
	})
	if err != nil {
		return nil, hit, err
	}
	trend, err := snapshotPayloadAs[[]usagestats.UserUsageTrendPoint](entry.Payload)
	return trend, hit, err
}
