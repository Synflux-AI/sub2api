package admin

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestUsageStatsCacheKey_StableAndDistinct(t *testing.T) {
	start := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	base := usagestats.UsageLogFilters{StartTime: &start, EndTime: &end, Model: "claude-3"}

	k1 := usageStatsCacheKey(base)
	k2 := usageStatsCacheKey(base)
	require.NotEmpty(t, k1)
	require.Equal(t, k1, k2, "same filters must produce same key")

	other := base
	other.Model = "gpt-4o"
	require.NotEqual(t, k1, usageStatsCacheKey(other), "different model must change key")

	withUser := base
	withUser.UserID = 7
	require.NotEqual(t, k1, usageStatsCacheKey(withUser), "different user must change key")
}

func TestUsageStatsCacheKey_NormalizesListsWithoutMutation(t *testing.T) {
	left := usagestats.UsageLogFilters{UserIDs: []int64{7, 2, 7}, Models: []string{"gpt-4o", "claude-3", "gpt-4o"}}
	right := usagestats.UsageLogFilters{UserIDs: []int64{2, 7}, Models: []string{"claude-3", "gpt-4o"}}

	require.Equal(t, usageStatsCacheKey(left), usageStatsCacheKey(right))
	require.Equal(t, []int64{7, 2, 7}, left.UserIDs)
	require.Equal(t, []string{"gpt-4o", "claude-3", "gpt-4o"}, left.Models)
}
