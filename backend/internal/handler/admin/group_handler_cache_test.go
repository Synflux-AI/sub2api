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

type groupUsageSummaryCacheProbe struct {
	service.UsageLogRepository
	calls atomic.Int32
}

func (r *groupUsageSummaryCacheProbe) GetAllGroupUsageSummary(context.Context, time.Time) ([]usagestats.GroupUsageSummary, error) {
	r.calls.Add(1)
	return []usagestats.GroupUsageSummary{{GroupID: 1, TotalCost: 10, TodayCost: 2}}, nil
}

func TestGroupHandler_GetUsageSummaryUsesTimezoneScopedCache(t *testing.T) {
	groupUsageSummaryCache = newSnapshotCache(30 * time.Second)
	t.Cleanup(func() { groupUsageSummaryCache = newSnapshotCache(30 * time.Second) })

	gin.SetMode(gin.TestMode)
	repo := &groupUsageSummaryCacheProbe{}
	dashboardSvc := service.NewDashboardService(repo, nil, nil, nil)
	handler := NewGroupHandler(nil, dashboardSvc, nil)
	router := gin.New()
	router.GET("/admin/groups/usage-summary", handler.GetUsageSummary)

	request := func(timezone string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/groups/usage-summary?timezone="+timezone, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec1 := request("UTC")
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, "miss", rec1.Header().Get("X-Snapshot-Cache"))

	rec2 := request("UTC")
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, "hit", rec2.Header().Get("X-Snapshot-Cache"))
	require.Equal(t, int32(1), repo.calls.Load())

	rec3 := request("Pacific/Auckland")
	require.Equal(t, http.StatusOK, rec3.Code)
	require.Equal(t, "miss", rec3.Header().Get("X-Snapshot-Cache"))
	require.Equal(t, int32(2), repo.calls.Load())
}
