package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestApplyUsageMultiFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?user_ids=2,1,2&group_ids=3,4&account_ids=5,6&models=gpt-5,claude-4", nil)

	filters := &usagestats.UsageLogFilters{}
	require.NoError(t, applyUsageMultiFilters(c, filters))
	require.Equal(t, []int64{2, 1}, filters.UserIDs)
	require.Equal(t, []int64{3, 4}, filters.GroupIDs)
	require.Equal(t, []int64{5, 6}, filters.AccountIDs)
	require.Equal(t, []string{"gpt-5", "claude-4"}, filters.Models)
}

func TestApplyUsageMultiFiltersRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?user_ids=1,nope", nil)
	require.Error(t, applyUsageMultiFilters(c, &usagestats.UsageLogFilters{}))
}
