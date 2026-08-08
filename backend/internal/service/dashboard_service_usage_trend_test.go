package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type dashboardUsageTrendFilterRepo struct {
	UsageLogRepository
	filters usagestats.UsageLogFilters
}

func (r *dashboardUsageTrendFilterRepo) GetUsageTrendWithUsageFilters(_ context.Context, _, _ time.Time, _ string, filters usagestats.UsageLogFilters) ([]usagestats.TrendDataPoint, error) {
	r.filters = filters
	return []usagestats.TrendDataPoint{}, nil
}

func TestDashboardServiceUsageTrendUsesRequestedModelAndScheduledAccount(t *testing.T) {
	repo := &dashboardUsageTrendFilterRepo{}
	service := &DashboardService{usageRepo: repo}

	_, err := service.GetUsageTrendWithFilters(context.Background(), time.Time{}, time.Time{}, "hour", 7, 11, 23, 29, "gpt-5", nil, nil, nil, true)
	require.NoError(t, err)
	require.Equal(t, int64(7), repo.filters.UserID)
	require.Equal(t, int64(23), repo.filters.AccountID)
	require.Equal(t, "gpt-5", repo.filters.Model)
	require.Equal(t, usagestats.ModelSourceRequested, repo.filters.ModelFilterSource)
	require.True(t, repo.filters.IncludeLatency)
}
