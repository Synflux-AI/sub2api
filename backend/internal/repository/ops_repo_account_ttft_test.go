package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestGetAccountModelTTFTPreservesGroupAndUpstreamModelCohort(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	start := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta("COALESCE(NULLIF(TRIM(u.upstream_model), ''), u.model) AS model")).
		WithArgs(start, end, 20).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "group_id", "account_name", "platform", "model", "ttft_p50_ms", "ttft_p95_ms", "samples",
		}).AddRow(int64(7), int64(42), "account-7", "anthropic", "claude-sonnet", 850.0, 1400.0, int64(31)))

	repo := &opsRepository{db: db}
	rows, err := repo.GetAccountModelTTFT(context.Background(), start, end, 20)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(42), rows[0].GroupID)
	require.Equal(t, "claude-sonnet", rows[0].Model)
	require.Equal(t, int64(31), rows[0].Samples)
	require.NoError(t, mock.ExpectationsWereMet())
}
