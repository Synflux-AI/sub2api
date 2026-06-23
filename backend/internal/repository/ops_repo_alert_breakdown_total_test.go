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

// error_rate 口径回归:total==sla(均为 status>=400 非业务限流),other==0,分母=success+sla。
// 钉住 default 分支不被误改:total=8(=4xx5+5xx3),sla=8,success=92 => 分母=100,other=0。
func TestGetAlertErrorBreakdown_ErrorRateDefaultUnchanged(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &opsRepository{db: db}
	mock.MatchExpectationsInOrder(false)

	start := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	mock.ExpectQuery(`COALESCE\(status_code,0\)>=400 AND NOT is_business_limited`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "c4xx", "c5xx", "sla"}).
			AddRow(int64(8), int64(5), int64(3), int64(8)))
	mock.ExpectQuery(`FROM usage_logs ul`).
		WillReturnRows(sqlmock.NewRows([]string{"success_count", "token_consumed"}).
			AddRow(int64(92), int64(0)))

	bd, err := repo.GetAlertErrorBreakdown(context.Background(), &service.OpsDashboardFilter{}, start, end, 5, "error_rate")
	require.NoError(t, err)
	require.Equal(t, int64(8), bd.TotalErrors)
	require.Equal(t, int64(5), bd.Client4xx)
	require.Equal(t, int64(3), bd.Server5xx)
	require.Equal(t, int64(0), bd.OtherErrors, "error_rate 口径下 total==4xx+5xx,无其他余量")
	require.Equal(t, int64(100), bd.WindowRequests, "分母 = success + sla")
	require.Equal(t, "error_rate", bd.MetricType)
}

// Top 上游按「逐次失败尝试」聚合:展开 upstream_errors JSONB 数组,按 event.account_id 计数,
// 排除 429/529(对齐错误率分子),且不带 model 维度(event 未存 model)。
// 一个请求里失败过 A、B 两个渠道时,A、B 各计一次。
func TestGetAlertErrorBreakdown_UpstreamTopPerAttempt(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &opsRepository{db: db}
	mock.MatchExpectationsInOrder(false)

	start := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	// 总数 + 分母(必须 stub,否则提前返回)。
	mock.ExpectQuery(`COALESCE\(status_code,0\)>=400 AND NOT is_business_limited`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "c4xx", "c5xx", "sla"}).
			AddRow(int64(15), int64(2), int64(1), int64(5)))
	mock.ExpectQuery(`FROM usage_logs ul`).
		WillReturnRows(sqlmock.NewRows([]string{"success_count", "token_consumed"}).
			AddRow(int64(87), int64(0)))

	// Top 上游:逐次聚合,SQL 必须展开 JSONB 数组并排除 429/529。
	mock.ExpectQuery(`jsonb_array_elements[\s\S]*NOT IN \(429,529\)`).
		WillReturnRows(sqlmock.NewRows([]string{"aid", "aname", "pf", "c"}).
			AddRow(int64(10), "chan-A", "anthropic", int64(1)).
			AddRow(int64(20), "chan-B", "anthropic", int64(1)))

	bd, err := repo.GetAlertErrorBreakdown(context.Background(), &service.OpsDashboardFilter{}, start, end, 5, "upstream_error_rate")
	require.NoError(t, err)
	require.Len(t, bd.TopUpstreams, 2, "A、B 各计一次")
	require.Equal(t, int64(10), bd.TopUpstreams[0].AccountID)
	require.Equal(t, "chan-A", bd.TopUpstreams[0].AccountName)
	require.Equal(t, int64(1), bd.TopUpstreams[0].Count)
	require.Empty(t, bd.TopUpstreams[0].Model, "逐次口径无 model 维度")
	require.Equal(t, int64(20), bd.TopUpstreams[1].AccountID)
}
