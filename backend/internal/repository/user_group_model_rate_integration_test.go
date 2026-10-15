//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupRepository_ModelRateMultipliersRoundTrip(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newGroupRepositoryWithSQL(tx.Client(), tx)

	g := &service.Group{
		Name: "model-rate-roundtrip", Platform: service.PlatformAnthropic, RateMultiplier: 0.8,
		Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard,
		ModelRateMultipliers: []service.GroupModelRateMultiplier{{Models: []string{"model-a", "model-b*"}, RateMultiplier: 0.6}},
	}
	require.NoError(t, repo.Create(ctx, g))

	got, err := repo.GetByIDLite(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, g.ModelRateMultipliers, got.ModelRateMultipliers)

	got.ModelRateMultipliers = nil
	require.NoError(t, repo.Update(ctx, got))
	var raw string
	require.NoError(t, scanSingleRow(ctx, tx, "SELECT model_rate_multipliers::text FROM groups WHERE id = $1", []any{g.ID}, &raw))
	require.JSONEq(t, `[]`, raw, "nil rules are stored as an empty array, matching the column default")
}

func TestUserGroupRateRepository_ModelRateOverrides(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := &userGroupRateRepository{sql: tx}

	user := mustCreateUser(t, client, &service.User{Email: "model-rate-user@example.com"})
	other := mustCreateUser(t, client, &service.User{Email: "model-rate-other@example.com"})
	group := mustCreateGroup(t, client, &service.Group{Name: "model-rate-overrides"})

	cfg, err := repo.GetRateConfigByUserAndGroup(ctx, user.ID, group.ID)
	require.NoError(t, err)
	require.Nil(t, cfg)

	// 只有模型覆盖、没有专属倍率的行。
	require.NoError(t, repo.SetUserGroupModelRates(ctx, user.ID, group.ID, map[string]float64{"model-a": 0.5, "model-b*": 0.7}))
	cfg, err = repo.GetRateConfigByUserAndGroup(ctx, user.ID, group.ID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Nil(t, cfg.RateMultiplier)
	require.Equal(t, map[string]float64{"model-a": 0.5, "model-b*": 0.7}, cfg.ModelRateMultipliers)

	// 批量同步专属倍率 / RPM 不得丢失模型覆盖，即使该用户不在本次列表里。
	require.NoError(t, repo.SyncGroupRateMultipliers(ctx, group.ID, []service.GroupRateMultiplierInput{{UserID: other.ID, RateMultiplier: 1.2}}))
	require.NoError(t, repo.SyncGroupRPMOverrides(ctx, group.ID, nil))
	require.NoError(t, repo.ClearGroupRPMOverrides(ctx, group.ID))
	require.NoError(t, repo.SyncUserGroupRates(ctx, user.ID, map[int64]*float64{}))
	cfg, err = repo.GetRateConfigByUserAndGroup(ctx, user.ID, group.ID)
	require.NoError(t, err)
	require.NotNil(t, cfg, "row with only model overrides must survive rate/rpm sync")
	require.Equal(t, map[string]float64{"model-a": 0.5, "model-b*": 0.7}, cfg.ModelRateMultipliers)

	// 设置专属倍率不覆盖模型覆盖；分组列表带出模型覆盖。
	rate := 0.4
	require.NoError(t, repo.SyncUserGroupRates(ctx, user.ID, map[int64]*float64{group.ID: &rate}))
	entries, err := repo.GetByGroupID(ctx, group.ID)
	require.NoError(t, err)
	byUser := map[int64]service.UserGroupRateEntry{}
	for _, e := range entries {
		byUser[e.UserID] = e
	}
	require.Equal(t, map[string]float64{"model-a": 0.5, "model-b*": 0.7}, byUser[user.ID].ModelRateMultipliers)
	require.NotNil(t, byUser[user.ID].RateMultiplier)
	require.Equal(t, 0.4, *byUser[user.ID].RateMultiplier)
	require.Nil(t, byUser[other.ID].ModelRateMultipliers)

	// 分组删掉 model-b* 规则：用户覆盖里对应键被清理，其余保留。
	require.NoError(t, repo.PruneGroupModelRates(ctx, group.ID, []string{"model-a"}))
	cfg, err = repo.GetRateConfigByUserAndGroup(ctx, user.ID, group.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]float64{"model-a": 0.5}, cfg.ModelRateMultipliers)
	require.Equal(t, 0.4, *cfg.RateMultiplier)

	// 全部恢复继承并清掉专属倍率后整行删除。
	require.NoError(t, repo.SetUserGroupModelRates(ctx, user.ID, group.ID, nil))
	require.NoError(t, repo.SyncUserGroupRates(ctx, user.ID, map[int64]*float64{group.ID: nil}))
	cfg, err = repo.GetRateConfigByUserAndGroup(ctx, user.ID, group.ID)
	require.NoError(t, err)
	require.Nil(t, cfg)

	// 清理到空：所有规则被删除时只剩模型覆盖的行也随之删除。
	require.NoError(t, repo.SetUserGroupModelRates(ctx, user.ID, group.ID, map[string]float64{"model-a": 0.5}))
	require.NoError(t, repo.PruneGroupModelRates(ctx, group.ID, nil))
	cfg, err = repo.GetRateConfigByUserAndGroup(ctx, user.ID, group.ID)
	require.NoError(t, err)
	require.Nil(t, cfg)
}
