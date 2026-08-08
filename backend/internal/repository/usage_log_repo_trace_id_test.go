package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestUsageLogRepositoryListWithFiltersTraceID(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery("SELECT .* FROM usage_logs WHERE trace_id = \\$1 ORDER BY id DESC LIMIT \\$2 OFFSET \\$3").
		WithArgs("trace-0123", 21, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	_, _, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 20}, usagestats.UsageLogFilters{
		TraceID: " trace-0123 ",
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
