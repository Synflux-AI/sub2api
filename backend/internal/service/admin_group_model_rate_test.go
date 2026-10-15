//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// userGroupModelRateAdminRepoStub 在分组倍率仓储桩之上实现模型倍率覆盖能力。
type userGroupModelRateAdminRepoStub struct {
	userGroupRateRepoStubForGroupRate

	setUserID, setGroupID int64
	setRates              map[string]float64
	setCalls              int

	prunedGroupID  int64
	prunedPatterns []string
	pruneCalls     int
}

func (s *userGroupModelRateAdminRepoStub) GetRateConfigByUserAndGroup(context.Context, int64, int64) (*UserGroupRateConfig, error) {
	return nil, nil
}

func (s *userGroupModelRateAdminRepoStub) SetUserGroupModelRates(_ context.Context, userID, groupID int64, rates map[string]float64) error {
	s.setCalls++
	s.setUserID, s.setGroupID, s.setRates = userID, groupID, rates
	return nil
}

func (s *userGroupModelRateAdminRepoStub) PruneGroupModelRates(_ context.Context, groupID int64, patterns []string) error {
	s.pruneCalls++
	s.prunedGroupID, s.prunedPatterns = groupID, patterns
	return nil
}

func modelRateAdminGroup() *Group {
	return &Group{
		ID: 9, Name: "model-rate", Platform: PlatformAnthropic, Status: StatusActive, RateMultiplier: 0.8,
		ModelRateMultipliers: []GroupModelRateMultiplier{
			{Models: []string{"model-small", "model-large-*"}, RateMultiplier: 0.6},
		},
	}
}

func TestAdminService_SetUserGroupModelRateMultipliers(t *testing.T) {
	group := modelRateAdminGroup()
	rateRepo := &userGroupModelRateAdminRepoStub{}
	svc := &adminServiceImpl{groupRepo: &groupRepoStubForAdmin{getByID: group}, userGroupRateRepo: rateRepo}
	ctx := context.Background()

	require.NoError(t, svc.SetUserGroupModelRateMultipliers(ctx, group.ID, 5, map[string]float64{"MODEL-SMALL": 0.5}))
	require.Equal(t, int64(5), rateRepo.setUserID)
	require.Equal(t, group.ID, rateRepo.setGroupID)
	require.Equal(t, map[string]float64{"model-small": 0.5}, rateRepo.setRates, "keys are stored as the group's own pattern")

	// 空 map 表示全部恢复继承。
	require.NoError(t, svc.SetUserGroupModelRateMultipliers(ctx, group.ID, 5, nil))
	require.Empty(t, rateRepo.setRates)

	err := svc.SetUserGroupModelRateMultipliers(ctx, group.ID, 5, map[string]float64{"model-unknown": 0.5})
	require.Error(t, err)
	require.Equal(t, int32(http.StatusBadRequest), infraerrors.FromError(err).Code)
	require.Equal(t, "USER_MODEL_RATE_PATTERN_UNKNOWN", infraerrors.FromError(err).Reason)

	err = svc.SetUserGroupModelRateMultipliers(ctx, group.ID, 5, map[string]float64{"model-small": 0})
	require.Error(t, err)
	require.Equal(t, 2, rateRepo.setCalls, "rejected input must not reach the repository")

	require.Error(t, svc.SetUserGroupModelRateMultipliers(ctx, group.ID, 0, nil))
}

func TestAdminService_SetUserGroupModelRateMultipliers_UnsupportedRepo(t *testing.T) {
	svc := &adminServiceImpl{groupRepo: &groupRepoStubForAdmin{getByID: modelRateAdminGroup()}, userGroupRateRepo: &userGroupRateRepoStubForGroupRate{}}
	err := svc.SetUserGroupModelRateMultipliers(context.Background(), 9, 5, map[string]float64{"model-small": 0.5})
	require.Error(t, err)
	require.Equal(t, int32(http.StatusServiceUnavailable), infraerrors.FromError(err).Code)
}

func TestAdminService_UpdateGroup_ModelRateMultipliersNormalizeAndPrune(t *testing.T) {
	existing := modelRateAdminGroup()
	groupRepo := &groupRepoStubForAdmin{getByID: existing}
	rateRepo := &userGroupModelRateAdminRepoStub{}
	svc := &adminServiceImpl{groupRepo: groupRepo, userGroupRateRepo: rateRepo}
	ctx := context.Background()

	// 不携带模型倍率时不改动、也不清理用户覆盖。
	_, err := svc.UpdateGroup(ctx, existing.ID, &UpdateGroupInput{Name: "renamed"})
	require.NoError(t, err)
	require.Equal(t, 0, rateRepo.pruneCalls)

	rules := []GroupModelRateMultiplier{{Models: []string{" model-small "}, RateMultiplier: 0.55}}
	_, err = svc.UpdateGroup(ctx, existing.ID, &UpdateGroupInput{ModelRateMultipliers: &rules})
	require.NoError(t, err)
	require.NotNil(t, groupRepo.updated)
	require.Equal(t, []GroupModelRateMultiplier{{Models: []string{"model-small"}, RateMultiplier: 0.55}}, groupRepo.updated.ModelRateMultipliers)
	require.Equal(t, 1, rateRepo.pruneCalls)
	require.Equal(t, existing.ID, rateRepo.prunedGroupID)
	require.Equal(t, []string{"model-small"}, rateRepo.prunedPatterns, "removed pattern model-large-* must be pruned from user overrides")

	bad := []GroupModelRateMultiplier{{Models: []string{"*"}, RateMultiplier: 1}}
	_, err = svc.UpdateGroup(ctx, existing.ID, &UpdateGroupInput{ModelRateMultipliers: &bad})
	require.Error(t, err)
	require.Equal(t, 1, rateRepo.pruneCalls)
}

func TestAdminService_CreateGroup_ModelRateMultipliers(t *testing.T) {
	repo := &groupRepoStubForAdmin{createID: 61}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name: "model-rate-group", Platform: PlatformAnthropic, RateMultiplier: 1,
		ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"model-a"}, RateMultiplier: 0.5}},
	})
	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.Equal(t, []GroupModelRateMultiplier{{Models: []string{"model-a"}, RateMultiplier: 0.5}}, repo.created.ModelRateMultipliers)

	repo.created = nil
	_, err = svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name: "model-rate-group-bad", Platform: PlatformAnthropic, RateMultiplier: 1,
		ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"model-a", "model-a"}, RateMultiplier: 0.5}},
	})
	require.Error(t, err)
	require.Nil(t, repo.created)
}

func TestCloneGroupForDuplicateCopiesModelRateMultipliers(t *testing.T) {
	source := modelRateAdminGroup()
	cloned := cloneGroupForDuplicate(source, "op-1")
	require.Equal(t, source.ModelRateMultipliers, cloned.ModelRateMultipliers)
	cloned.ModelRateMultipliers[0].Models[0] = "mutated"
	require.Equal(t, "model-small", source.ModelRateMultipliers[0].Models[0], "clone must not alias the source slice")
}
