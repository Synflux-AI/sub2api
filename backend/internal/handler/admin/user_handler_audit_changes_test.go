package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUserChangesAuditExtra(t *testing.T) {
	old, now := 0.3, 0.45
	require.Equal(t, map[string]any{
		"changed":         "status,concurrency,allowed_groups,group_rates",
		"old_status":      "active",
		"old_concurrency": 5,
		"groups_added":    "104",
		"groups_removed":  "101,105",
		"rates_changed":   "101:0.3>,103:>0.45,106:0.3>0.45",
	}, userChangesAuditExtra(service.UserChanges{
		Fields:        []string{"status", "concurrency", "allowed_groups", "group_rates"},
		Old:           map[string]any{"status": "active", "concurrency": 5},
		GroupsAdded:   []int64{104},
		GroupsRemoved: []int64{101, 105},
		Rates: []service.UserGroupRateChange{
			{GroupID: 101, Old: &old},
			{GroupID: 103, New: &now},
			{GroupID: 106, Old: &old, New: &now},
		},
	}))
	// 什么都没变也写 changed，表示这条记录有改前值可比
	require.Equal(t, map[string]any{"changed": ""}, userChangesAuditExtra(service.UserChanges{}))
}
