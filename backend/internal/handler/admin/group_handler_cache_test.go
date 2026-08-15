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

// 分组用量汇总改由服务端时区的预聚合 rollup 提供（migrations 222/223），
// 日界由 service.GroupUsageTodayStart 统一裁定，缓存键只跟随该日界。
// 客户端 timezone 参数不再参与分桶——rollup 是按服务端时区聚合的，
// 无法按任意客户端时区重切，若某次改动让该参数重新影响缓存键，此用例会失败。
func TestGroupHandler_GetUsageSummaryCacheKeyedByServerDayNotClientTimezone(t *testing.T) {
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

	// 换客户端时区仍命中同一条目：日界由服务端配置时区决定，与该参数无关。
	rec3 := request("Pacific/Auckland")
	require.Equal(t, http.StatusOK, rec3.Code)
	require.Equal(t, "hit", rec3.Header().Get("X-Snapshot-Cache"))
	require.Equal(t, int32(1), repo.calls.Load())
}
