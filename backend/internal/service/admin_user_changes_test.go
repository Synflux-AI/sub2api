//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminService_UpdateUser_ReportsChanges(t *testing.T) {
	rateRepo := &userGroupRateRepoStubForGroupRate{
		userRates: map[int64]map[int64]float64{42: {101: 0.3, 103: 1.1}},
	}
	svc := &adminServiceImpl{
		userRepo: &userRepoStub{user: &User{
			ID: 42, Email: "u@example.com", Status: StatusActive, Concurrency: 5, Notes: "n", AllowedGroups: []int64{101, 102},
		}},
		userGroupRateRepo: rateRepo,
		redeemCodeRepo:    &redeemRepoStub{},
	}

	// 取消 101 并清掉它的倍率、授权 104、把 103 改成 0.45；表单整张提交，备注与并发没变
	newRate, concurrency, notes := 0.45, 5, "n"
	allowed := []int64{102, 104}
	var changes UserChanges
	_, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		Status:        "disabled",
		Concurrency:   &concurrency,
		Notes:         &notes,
		AllowedGroups: &allowed,
		GroupRates:    map[int64]*float64{101: nil, 103: &newRate, 102: nil},
		Changes:       &changes,
	})
	require.NoError(t, err)

	oldA, oldC := 0.3, 1.1
	require.Equal(t, []string{"status", "allowed_groups", "group_rates"}, changes.Fields)
	require.Equal(t, map[string]any{"status": StatusActive}, changes.Old)
	require.Equal(t, []int64{104}, changes.GroupsAdded)
	require.Equal(t, []int64{101}, changes.GroupsRemoved)
	// 102 本来就没有专属倍率，提交 null 不算变化
	require.Equal(t, []UserGroupRateChange{
		{GroupID: 101, Old: &oldA},
		{GroupID: 103, Old: &oldC, New: &newRate},
	}, changes.Rates)
}

func TestAdminService_UpdateUser_NoChanges(t *testing.T) {
	svc := &adminServiceImpl{
		userRepo:       &userRepoStub{user: &User{ID: 42, Email: "u@example.com", Status: StatusActive, AllowedGroups: []int64{102}}},
		redeemCodeRepo: &redeemRepoStub{},
	}
	allowed := []int64{102}
	var changes UserChanges
	_, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{Status: StatusActive, AllowedGroups: &allowed, Changes: &changes})
	require.NoError(t, err)
	require.Empty(t, changes.Fields)
	require.Empty(t, changes.Old)
}
