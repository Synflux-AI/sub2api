package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// upstream 口径:total=15(含恢复行),4xx=2,5xx=1,余量other=12;sla=5,success=87 => 分母=92。
func TestGetAlertErrorBreakdown_UpstreamDenominatorAndBuckets(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &opsRepository{db: db}
	mock.MatchExpectationsInOrder(false)

	start := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	// 总数查询:4 列 total / c4xx / c5xx / sla
	mock.ExpectQuery(`COALESCE\(status_code,0\)>=400 AND NOT is_business_limited`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "c4xx", "c5xx", "sla"}).
			AddRow(int64(15), int64(2), int64(1), int64(5)))
	// 分母用的成功数查询
	mock.ExpectQuery(`FROM usage_logs ul`).
		WillReturnRows(sqlmock.NewRows([]string{"success_count", "token_consumed"}).
			AddRow(int64(87), int64(0)))

	bd, err := repo.GetAlertErrorBreakdown(context.Background(), &service.OpsDashboardFilter{}, start, end, 5, "upstream_error_rate")
	require.NoError(t, err)
	require.Equal(t, int64(15), bd.TotalErrors)
	require.Equal(t, int64(2), bd.Client4xx)
	require.Equal(t, int64(1), bd.Server5xx)
	require.Equal(t, int64(12), bd.OtherErrors, "other = total - 4xx - 5xx")
	require.Equal(t, int64(92), bd.WindowRequests, "分母必须是 success + sla,不是 success + total")
	require.Equal(t, "upstream_error_rate", bd.MetricType)
}
