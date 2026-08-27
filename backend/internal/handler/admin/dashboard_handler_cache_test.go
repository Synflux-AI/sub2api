package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type dashboardUsageRepoCacheProbe struct {
	service.UsageLogRepository
	trendCalls        atomic.Int32
	trendFilters      []usagestats.UsageLogFilters
	modelTrendFilters []usagestats.UsageLogFilters
	usersTrendCalls   atomic.Int32
	usersTrendSorts   []string
	usersTrendFilters []usagestats.UsageLogFilters
}

func (r *dashboardUsageRepoCacheProbe) GetUsageTrendByModelWithUsageFilters(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	filters usagestats.UsageLogFilters,
) ([]usagestats.TrendModelDataPoint, error) {
	r.modelTrendFilters = append(r.modelTrendFilters, filters)
	return []usagestats.TrendModelDataPoint{}, nil
}

func (r *dashboardUsageRepoCacheProbe) GetUsageTrendByModelWithFilters(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	userID, apiKeyID, accountID, groupID int64,
	model string,
	requestType *int16,
	stream *bool,
	billingType *int8,
	upstreamModelMismatch *bool,
) ([]usagestats.TrendModelDataPoint, error) {
	r.modelTrendFilters = append(r.modelTrendFilters, usagestats.UsageLogFilters{Stream: stream})
	return []usagestats.TrendModelDataPoint{}, nil
}

func (r *dashboardUsageRepoCacheProbe) GetUsageTrendWithUsageFilters(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	filters usagestats.UsageLogFilters,
) ([]usagestats.TrendDataPoint, error) {
	r.trendFilters = append(r.trendFilters, filters)
	return r.GetUsageTrendWithFilters(ctx, startTime, endTime, granularity, filters.UserID, filters.APIKeyID, filters.AccountID, filters.GroupID, filters.Model, filters.RequestType, filters.Stream, filters.BillingType)
}

func (r *dashboardUsageRepoCacheProbe) GetUsageTrendWithFilters(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	userID, apiKeyID, accountID, groupID int64,
	model string,
	requestType *int16,
	stream *bool,
	billingType *int8,
) ([]usagestats.TrendDataPoint, error) {
	r.trendCalls.Add(1)
	return []usagestats.TrendDataPoint{{
		Date:        "2026-03-11",
		Requests:    1,
		TotalTokens: 2,
		Cost:        3,
		ActualCost:  4,
	}}, nil
}

func (r *dashboardUsageRepoCacheProbe) GetUserUsageTrend(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	limit int,
	sortBy string,
) ([]usagestats.UserUsageTrendPoint, error) {
	r.usersTrendCalls.Add(1)
	r.usersTrendSorts = append(r.usersTrendSorts, sortBy)
	return []usagestats.UserUsageTrendPoint{{
		Date:       "2026-03-11",
		UserID:     1,
		Email:      "cache@test.dev",
		Requests:   2,
		Tokens:     20,
		Cost:       2,
		ActualCost: 1,
	}}, nil
}

func (r *dashboardUsageRepoCacheProbe) GetUserUsageTrendWithUsageFilters(
	ctx context.Context,
	startTime, endTime time.Time,
	granularity string,
	limit int,
	sortBy string,
	filters usagestats.UsageLogFilters,
) ([]usagestats.UserUsageTrendPoint, error) {
	r.usersTrendFilters = append(r.usersTrendFilters, filters)
	return r.GetUserUsageTrend(ctx, startTime, endTime, granularity, limit, sortBy)
}

func resetDashboardReadCachesForTest() {
	dashboardTrendCache = newSnapshotCache(30 * time.Second)
	dashboardUsersTrendCache = newSnapshotCache(30 * time.Second)
	dashboardAPIKeysTrendCache = newSnapshotCache(30 * time.Second)
	dashboardModelStatsCache = newSnapshotCache(30 * time.Second)
	dashboardGroupStatsCache = newSnapshotCache(30 * time.Second)
	dashboardSnapshotV2Cache = newSnapshotCache(30 * time.Second)
}

func TestDashboardHandler_GetUsageTrend_UsesCache(t *testing.T) {
	t.Cleanup(resetDashboardReadCachesForTest)
	resetDashboardReadCachesForTest()

	gin.SetMode(gin.TestMode)
	repo := &dashboardUsageRepoCacheProbe{}
	dashboardSvc := service.NewDashboardService(repo, nil, nil, nil)
	handler := NewDashboardHandler(dashboardSvc, nil)
	router := gin.New()
	router.GET("/admin/dashboard/trend", handler.GetUsageTrend)

	req1 := httptest.NewRequest(http.MethodGet, "/admin/dashboard/trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day", nil)
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, "miss", rec1.Header().Get("X-Snapshot-Cache"))

	req2 := httptest.NewRequest(http.MethodGet, "/admin/dashboard/trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, "hit", rec2.Header().Get("X-Snapshot-Cache"))
	require.Equal(t, int32(1), repo.trendCalls.Load())

	req3 := httptest.NewRequest(http.MethodGet, "/admin/dashboard/trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day&output_tokens=0&stream=false", nil)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusOK, rec3.Code)
	require.Equal(t, "miss", rec3.Header().Get("X-Snapshot-Cache"))
	require.Equal(t, int32(2), repo.trendCalls.Load())
	require.NotNil(t, repo.trendFilters[1].OutputTokens)
	require.Zero(t, *repo.trendFilters[1].OutputTokens)
	require.NotNil(t, repo.trendFilters[1].Stream)
	require.False(t, *repo.trendFilters[1].Stream)

	req4 := httptest.NewRequest(http.MethodGet, "/admin/dashboard/trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day&group_by=model&output_tokens=0&stream=false", nil)
	rec4 := httptest.NewRecorder()
	router.ServeHTTP(rec4, req4)
	require.Equal(t, http.StatusOK, rec4.Code)
	require.Len(t, repo.modelTrendFilters, 1)
	require.NotNil(t, repo.modelTrendFilters[0].OutputTokens)
	require.Zero(t, *repo.modelTrendFilters[0].OutputTokens)
	require.NotNil(t, repo.modelTrendFilters[0].Stream)
	require.False(t, *repo.modelTrendFilters[0].Stream)
}

func TestDashboardHandler_GetUsageTrend_CachesLatencySeparately(t *testing.T) {
	t.Cleanup(resetDashboardReadCachesForTest)
	resetDashboardReadCachesForTest()

	gin.SetMode(gin.TestMode)
	repo := &dashboardUsageRepoCacheProbe{}
	dashboardSvc := service.NewDashboardService(repo, nil, nil, nil)
	handler := NewDashboardHandler(dashboardSvc, nil)
	router := gin.New()
	router.GET("/admin/dashboard/trend", handler.GetUsageTrend)

	for _, target := range []string{
		"/admin/dashboard/trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day",
		"/admin/dashboard/trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day&include_latency=true",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "miss", rec.Header().Get("X-Snapshot-Cache"))
	}
	require.Equal(t, int32(2), repo.trendCalls.Load())
}

func TestDashboardHandler_GetUsageTrend_CachedModelUsesRequestedSource(t *testing.T) {
	t.Cleanup(resetDashboardReadCachesForTest)
	resetDashboardReadCachesForTest()

	gin.SetMode(gin.TestMode)
	repo := &dashboardUsageRepoCacheProbe{}
	handler := NewDashboardHandler(service.NewDashboardService(repo, nil, nil, nil), nil)
	router := gin.New()
	router.GET("/admin/dashboard/trend", handler.GetUsageTrend)

	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/trend?start_date=2026-03-01&end_date=2026-03-07&model=public-model", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.trendFilters, 1)
	require.Equal(t, "public-model", repo.trendFilters[0].Model)
	require.Equal(t, usagestats.ModelSourceRequested, repo.trendFilters[0].ModelFilterSource)
}

func TestDashboardHandler_GetUserUsageTrend_UsesCache(t *testing.T) {
	t.Cleanup(resetDashboardReadCachesForTest)
	resetDashboardReadCachesForTest()

	gin.SetMode(gin.TestMode)
	repo := &dashboardUsageRepoCacheProbe{}
	dashboardSvc := service.NewDashboardService(repo, nil, nil, nil)
	handler := NewDashboardHandler(dashboardSvc, nil)
	router := gin.New()
	router.GET("/admin/dashboard/users-trend", handler.GetUserUsageTrend)

	req1 := httptest.NewRequest(http.MethodGet, "/admin/dashboard/users-trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day&limit=8", nil)
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, "miss", rec1.Header().Get("X-Snapshot-Cache"))

	req2 := httptest.NewRequest(http.MethodGet, "/admin/dashboard/users-trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day&limit=8", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, "hit", rec2.Header().Get("X-Snapshot-Cache"))
	require.Equal(t, int32(1), repo.usersTrendCalls.Load())

	req3 := httptest.NewRequest(http.MethodGet, "/admin/dashboard/users-trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day&limit=8&output_tokens=0&stream=false", nil)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusOK, rec3.Code)
	require.Equal(t, "miss", rec3.Header().Get("X-Snapshot-Cache"))
	require.Equal(t, int32(2), repo.usersTrendCalls.Load())
	require.NotNil(t, repo.usersTrendFilters[1].OutputTokens)
	require.Zero(t, *repo.usersTrendFilters[1].OutputTokens)
	require.NotNil(t, repo.usersTrendFilters[1].Stream)
	require.False(t, *repo.usersTrendFilters[1].Stream)
}

func TestDashboardHandler_GetUserUsageTrend_CachesSortSeparately(t *testing.T) {
	t.Cleanup(resetDashboardReadCachesForTest)
	resetDashboardReadCachesForTest()

	gin.SetMode(gin.TestMode)
	repo := &dashboardUsageRepoCacheProbe{}
	handler := NewDashboardHandler(service.NewDashboardService(repo, nil, nil, nil), nil)
	router := gin.New()
	router.GET("/admin/dashboard/users-trend", handler.GetUserUsageTrend)

	for _, sortBy := range []string{"actual_cost", "total_tokens"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/users-trend?start_date=2026-03-01&end_date=2026-03-07&granularity=day&limit=8&sort_by="+sortBy, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "miss", rec.Header().Get("X-Snapshot-Cache"))
	}
	require.Equal(t, int32(2), repo.usersTrendCalls.Load())
	require.Equal(t, []string{"actual_cost", "total_tokens"}, repo.usersTrendSorts)
}

func TestDashboardHandler_GetUserUsageTrend_AppliesFiltersAndNormalizesCacheKey(t *testing.T) {
	t.Cleanup(resetDashboardReadCachesForTest)
	resetDashboardReadCachesForTest()

	gin.SetMode(gin.TestMode)
	repo := &dashboardUsageRepoCacheProbe{}
	handler := NewDashboardHandler(service.NewDashboardService(repo, nil, nil, nil), nil)
	router := gin.New()
	router.GET("/admin/dashboard/users-trend", handler.GetUserUsageTrend)

	for _, users := range []string{"7,2", "2,7"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/users-trend?start_date=2026-03-01&end_date=2026-03-07&user_id="+users+"&group_id=4&model=public-model", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	require.Equal(t, int32(1), repo.usersTrendCalls.Load())
	require.Len(t, repo.usersTrendFilters, 1)
	require.Equal(t, []int64{7, 2}, repo.usersTrendFilters[0].UserIDs)
	require.Equal(t, int64(4), repo.usersTrendFilters[0].GroupID)
	require.Equal(t, "public-model", repo.usersTrendFilters[0].Model)
	require.Equal(t, usagestats.ModelSourceRequested, repo.usersTrendFilters[0].ModelFilterSource)
}
