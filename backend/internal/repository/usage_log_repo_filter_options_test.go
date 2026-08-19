//go:build unit

package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestGetUsageFilterOptionsReadsAllFacets(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	mock.ExpectQuery(`SELECT DISTINCT ul.user_id`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "email", "notes"}).AddRow(int64(7), "user@example.com", "VIP"))
	mock.ExpectQuery(`SELECT DISTINCT COALESCE`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"model"}).AddRow("public-model"))
	mock.ExpectQuery(`SELECT DISTINCT ul.group_id`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"group_id", "name", "platform"}).AddRow(int64(3), "Primary", "anthropic"))
	mock.ExpectQuery(`SELECT DISTINCT ul.account_id`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "name", "platform"}).AddRow(int64(9), "Upstream", "anthropic"))

	options, err := repo.GetUsageFilterOptions(context.Background(), start, end, UsageLogFilters{})
	require.NoError(t, err)
	require.Equal(t, int64(7), options.Users[0].ID)
	require.Equal(t, "public-model", options.Models[0])
	require.Equal(t, int64(3), options.Groups[0].ID)
	require.Equal(t, int64(9), options.Accounts[0].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetUsageFilterOptionsPropagatesRowError(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	wantErr := errors.New("account facet interrupted")

	mock.ExpectQuery(`SELECT DISTINCT ul.user_id`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "email", "notes"}).AddRow(int64(7), "user@example.com", ""))
	mock.ExpectQuery(`SELECT DISTINCT COALESCE`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"model"}).AddRow("public-model"))
	mock.ExpectQuery(`SELECT DISTINCT ul.group_id`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"group_id", "name", "platform"}).AddRow(int64(3), "Primary", "anthropic"))
	mock.ExpectQuery(`SELECT DISTINCT ul.account_id`).WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "name", "platform"}).AddRow(int64(9), "Upstream", "anthropic").RowError(0, wantErr))

	options, err := repo.GetUsageFilterOptions(context.Background(), start, end, UsageLogFilters{})
	require.Nil(t, options)
	require.ErrorIs(t, err, wantErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageFilterOptionsWhereExcludesOnlyCurrentDimension(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	filters := UsageLogFilters{
		UserID: 1, APIKeyID: 2, AccountID: 3, GroupID: 4,
		Model: "public-model", ModelFilterSource: usagestats.ModelSourceRequested,
	}
	columns := map[string]string{
		"user": "ul.user_id", "account": "ul.account_id", "group": "ul.group_id",
		"model": resolveModelDimensionExpressionWithAlias(usagestats.ModelSourceRequested, "ul"),
	}

	for dimension, excludedColumn := range columns {
		t.Run(dimension, func(t *testing.T) {
			where, args := usageFilterOptionsWhere(start, end, filters, dimension)
			require.NotContains(t, where, excludedColumn+" =")
			require.Contains(t, where, "ul.api_key_id =")
			for otherDimension, column := range columns {
				if otherDimension != dimension {
					require.Contains(t, where, column+" =")
				}
			}
			require.Len(t, args, 6)
		})
	}
}

func TestUserTrendAndModelTrendUseSharedRequestedModelFilters(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	t.Run("user trend", func(t *testing.T) {
		db, mock := newSQLMock(t)
		repo := &usageLogRepository{sql: db}
		filters := UsageLogFilters{
			UserIDs: []int64{7, 8}, AccountID: 3, GroupID: 4,
			Models: []string{"public-model"}, ModelFilterSource: usagestats.ModelSourceRequested,
		}
		mock.ExpectQuery(`(?s)WITH filtered AS .*user_id IN \(\$3, \$4\).*account_id = \$5.*group_id = \$6.*COALESCE\(NULLIF\(TRIM\(requested_model\), ''\), model\) IN \(\$7\).*FROM filtered u`).
			WithArgs(start, end, int64(7), int64(8), int64(3), int64(4), "public-model", 8).
			WillReturnRows(sqlmock.NewRows([]string{
				"date", "user_id", "key", "label", "email", "username", "notes", "requests", "tokens", "cost", "actual_cost",
			}))

		rows, err := repo.GetUserUsageTrendWithUsageFilters(context.Background(), start, end, "day", 8, "total_tokens", filters)
		require.NoError(t, err)
		require.Empty(t, rows)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("model trend", func(t *testing.T) {
		db, mock := newSQLMock(t)
		repo := &usageLogRepository{sql: db}
		modelExpr := regexp.QuoteMeta(resolveModelDimensionExpression(usagestats.ModelSourceRequested))
		mock.ExpectQuery(`(?s)SELECT.*`+modelExpr+` AS model.*WHERE.*`+modelExpr+` IN \(\$3\).*GROUP BY date, `+modelExpr).
			WithArgs(start, end, "public-model").
			WillReturnRows(sqlmock.NewRows([]string{
				"date", "model", "requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost", "actual_cost",
			}))

		rows, err := repo.GetUsageTrendByModelWithUsageFilters(context.Background(), start, end, "day", UsageLogFilters{
			Models: []string{"public-model"},
		})
		require.NoError(t, err)
		require.Empty(t, rows)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
