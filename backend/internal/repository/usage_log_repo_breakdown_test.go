//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestResolveEndpointColumn(t *testing.T) {
	tests := []struct {
		endpointType string
		want         string
	}{
		{"inbound", "ul.inbound_endpoint"},
		{"upstream", "ul.upstream_endpoint"},
		{"path", "ul.inbound_endpoint || ' -> ' || ul.upstream_endpoint"},
		{"", "ul.inbound_endpoint"},        // default
		{"unknown", "ul.inbound_endpoint"}, // fallback
	}

	for _, tc := range tests {
		t.Run(tc.endpointType, func(t *testing.T) {
			got := resolveEndpointColumn(tc.endpointType)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestResolveModelDimensionExpression(t *testing.T) {
	tests := []struct {
		modelType string
		want      string
	}{
		{usagestats.ModelSourceRequested, "COALESCE(NULLIF(TRIM(requested_model), ''), model)"},
		{usagestats.ModelSourceUpstream, "COALESCE(NULLIF(TRIM(upstream_model), ''), model)"},
		{usagestats.ModelSourceMapping, "(COALESCE(NULLIF(TRIM(requested_model), ''), model) || ' -> ' || COALESCE(NULLIF(TRIM(upstream_model), ''), model))"},
		{"", "COALESCE(NULLIF(TRIM(requested_model), ''), model)"},
		{"invalid", "COALESCE(NULLIF(TRIM(requested_model), ''), model)"},
	}

	for _, tc := range tests {
		t.Run(tc.modelType, func(t *testing.T) {
			got := resolveModelDimensionExpression(tc.modelType)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestGetUserBreakdownStatsRequestTypeIncludesLegacyFallback(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	requestType := int16(service.RequestTypeStream)

	legacyFilter := `(ul.request_type = $3 OR (ul.request_type = 0 AND ul.stream = TRUE AND ul.openai_ws_mode = FALSE))`
	mock.ExpectQuery(regexp.QuoteMeta(legacyFilter)).
		WithArgs(start, end, requestType).
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id", "email", "requests", "input_tokens", "output_tokens",
			"cache_tokens", "total_tokens", "cost", "actual_cost", "account_cost",
		}))

	rows, err := repo.GetUserBreakdownStats(context.Background(), start, end, usagestats.UserBreakdownDimension{
		RequestType: &requestType,
	}, 0)

	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetUserUsageTrendSort(t *testing.T) {
	for _, tc := range []struct {
		sortBy  string
		orderBy string
	}{
		{"actual_cost", "SUM(actual_cost)"},
		{"requests", "COUNT(*)"},
		{"total_tokens", "SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens)"},
		{"invalid", "SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens)"},
	} {
		t.Run(tc.sortBy, func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			end := start.Add(24 * time.Hour)

			mock.ExpectQuery(regexp.QuoteMeta("ORDER BY "+tc.orderBy+" DESC, user_id ASC")).
				WithArgs(start, end, 8).
				WillReturnRows(sqlmock.NewRows([]string{
					"date", "user_id", "email", "username", "notes", "requests", "tokens", "cost", "actual_cost",
				}))

			rows, err := repo.GetUserUsageTrend(context.Background(), start, end, "day", 8, tc.sortBy, usagestats.UsageLogFilters{})
			require.NoError(t, err)
			require.Empty(t, rows)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestGetUserUsageTrendIncludesOthers(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	mock.ExpectQuery("__others__").
		WithArgs(start, end, 8).
		WillReturnRows(sqlmock.NewRows([]string{
			"date", "user_id", "key", "label", "email", "username", "notes", "requests", "tokens", "cost", "actual_cost",
		}).AddRow("2026-07-01", 0, "__others__", "其他", "", "", "", 3, 300, 1.5, 1.2))

	rows, err := repo.GetUserUsageTrend(context.Background(), start, end, "day", 8, "total_tokens", usagestats.UsageLogFilters{})
	require.NoError(t, err)
	require.Equal(t, []UserUsageTrendPoint{{
		Date: "2026-07-01", UserID: 0, Key: "__others__", Label: "其他",
		Requests: 3, Tokens: 300, Cost: 1.5, ActualCost: 1.2,
	}}, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetUserUsageTrendFiltersBeforeRankingAndBuckets(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	mock.ExpectQuery(`WITH filtered_logs AS \( SELECT \* FROM usage_logs WHERE created_at >= \$1 AND created_at < \$2 AND user_id = \$3 AND api_key_id = \$4 AND account_id = \$5 AND group_id = \$6 AND COALESCE\(NULLIF\(TRIM\(requested_model\), ''\), model\) = \$7 \), top_users AS \( SELECT user_id FROM filtered_logs.*FROM filtered_logs u`).
		WithArgs(start, end, int64(11), int64(22), int64(33), int64(44), "gpt-5", 8).
		WillReturnRows(sqlmock.NewRows([]string{
			"date", "user_id", "key", "label", "email", "username", "notes", "requests", "tokens", "cost", "actual_cost",
		}))

	rows, err := repo.GetUserUsageTrend(context.Background(), start, end, "day", 8, "total_tokens", usagestats.UsageLogFilters{
		UserID: 11, APIKeyID: 22, AccountID: 33, GroupID: 44, Model: "gpt-5", ModelFilterSource: usagestats.ModelSourceRequested,
	})
	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}
