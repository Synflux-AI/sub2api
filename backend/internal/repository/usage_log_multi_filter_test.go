package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogMultiFilterConditionsUseAny(t *testing.T) {
	conditions := []string{}
	args := []any{}
	conditions, args = appendUsageLogIDWhereCondition(conditions, args, "user_id", 9, []int64{2, 1, 2})
	conditions, args = appendUsageLogIDWhereCondition(conditions, args, "group_id", 0, []int64{3, 4})
	conditions, args = appendUsageLogModelsWhereCondition(conditions, args, "ignored", []string{"gpt-5", "claude-4"}, usagestats.ModelSourceRequested)

	require.Equal(t, []string{
		"user_id = ANY($1)",
		"group_id = ANY($2)",
		"COALESCE(NULLIF(TRIM(requested_model), ''), model) = ANY($3)",
	}, conditions)
	require.Len(t, args, 3)
}

func TestOpsMultiFilterConditionsUseAny(t *testing.T) {
	filter := &service.OpsDashboardFilter{
		UserIDs: []int64{1, 2}, GroupIDs: []int64{3, 4}, AccountIDs: []int64{5, 6}, Models: []string{"gpt-5", "claude-4"},
	}
	where, _, _ := buildErrorWhere(filter, testTime(1), testTime(2), 1)
	for _, want := range []string{"user_id = ANY($", "group_id = ANY($", "account_id = ANY($", "COALESCE(requested_model, model, '') = ANY($"} {
		require.True(t, strings.Contains(where, want), "where missing %q: %s", want, where)
	}
}

func testTime(hour int) (out time.Time) {
	return time.Date(2026, 8, 1, hour, 0, 0, 0, time.UTC)
}
