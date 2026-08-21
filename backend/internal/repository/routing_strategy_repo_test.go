package repository

import (
	"context"
	"database/sql"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

// newRoutingStrategyRepoSQLite 用内存 SQLite 支撑的 ent client 构造仓储，
// 覆盖 Create/Update 对 group_id（回滚窗口保留列）与 group_ids 的同步不变量。
func newRoutingStrategyRepoSQLite(t *testing.T) (service.RoutingStrategyRepository, *dbent.Client) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:routing_strategy_repo_test?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)

	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })

	return NewRoutingStrategyRepository(client), client
}

func minimalRoutingStrategy(name string, groupIDs []int64) *service.RoutingStrategy {
	return &service.RoutingStrategy{
		Name:              name,
		Enabled:           true,
		Priority:          100,
		Platform:          service.PlatformAnthropic,
		GroupIDs:          groupIDs,
		MatchMode:         service.RoutingMatchModeAll,
		Action:            service.RoutingActionRestrict,
		AccountIDs:        []int64{1},
		AccountPriorities: []int{0},
	}
}

func TestRoutingStrategyRepository_CreateSyncsLegacyGroupIDToFirstElement(t *testing.T) {
	repo, client := newRoutingStrategyRepoSQLite(t)
	ctx := context.Background()

	st := minimalRoutingStrategy("create-multi-group", []int64{5, 7, 9})
	require.NoError(t, repo.Create(ctx, st))
	require.NotZero(t, st.ID)

	m, err := client.RoutingStrategy.Get(ctx, st.ID)
	require.NoError(t, err)
	require.NotNil(t, m.GroupID, "legacy group_id column must be set for rollback window")
	require.Equal(t, st.GroupIDs[0], *m.GroupID)
	require.Equal(t, []int64{5, 7, 9}, m.GroupIds)
}

func TestRoutingStrategyRepository_CreateWithEmptyGroupIDsLeavesLegacyColumnNull(t *testing.T) {
	repo, client := newRoutingStrategyRepoSQLite(t)
	ctx := context.Background()

	st := minimalRoutingStrategy("create-global", nil)
	require.NoError(t, repo.Create(ctx, st))

	m, err := client.RoutingStrategy.Get(ctx, st.ID)
	require.NoError(t, err)
	require.Nil(t, m.GroupID, "empty group_ids must leave legacy group_id NULL")
	require.Empty(t, m.GroupIds)
}

func TestRoutingStrategyRepository_UpdateSyncsLegacyGroupIDToFirstElement(t *testing.T) {
	repo, client := newRoutingStrategyRepoSQLite(t)
	ctx := context.Background()

	st := minimalRoutingStrategy("update-target", []int64{1})
	require.NoError(t, repo.Create(ctx, st))

	st.GroupIDs = []int64{11, 22}
	require.NoError(t, repo.Update(ctx, st))

	m, err := client.RoutingStrategy.Get(ctx, st.ID)
	require.NoError(t, err)
	require.NotNil(t, m.GroupID)
	require.Equal(t, int64(11), *m.GroupID)
	require.Equal(t, []int64{11, 22}, m.GroupIds)
}

func TestRoutingStrategyRepository_UpdateClearingGroupIDsNullsLegacyColumn(t *testing.T) {
	repo, client := newRoutingStrategyRepoSQLite(t)
	ctx := context.Background()

	st := minimalRoutingStrategy("update-clear", []int64{42})
	require.NoError(t, repo.Create(ctx, st))

	m, err := client.RoutingStrategy.Get(ctx, st.ID)
	require.NoError(t, err)
	require.NotNil(t, m.GroupID, "sanity check: legacy column set right after create")

	st.GroupIDs = nil
	require.NoError(t, repo.Update(ctx, st))

	m, err = client.RoutingStrategy.Get(ctx, st.ID)
	require.NoError(t, err)
	require.Nil(t, m.GroupID, "clearing group_ids must NULL out the legacy group_id column")
	require.Empty(t, m.GroupIds)
}

func TestRoutingStrategyRepository_GetByIDOnlyReadsGroupIDs(t *testing.T) {
	repo, client := newRoutingStrategyRepoSQLite(t)
	ctx := context.Background()

	// 模拟迁移前的历史脏数据：group_id 有值但 group_ids 已被写空（不该发生，但读路径不能兜底回落）。
	created, err := client.RoutingStrategy.Create().
		SetName("stale-legacy-only").
		SetEnabled(true).
		SetPriority(100).
		SetPlatform(service.PlatformAnthropic).
		SetMatchMode(service.RoutingMatchModeAll).
		SetAction(service.RoutingActionRestrict).
		SetAccountIds([]int64{1}).
		SetAccountPriorities([]int{0}).
		SetGroupID(99).
		SetGroupIds([]int64{}).
		Save(ctx)
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, created.ID)
	require.NoError(t, err)
	require.Empty(t, got.GroupIDs, "read path must only trust group_ids, never fall back to legacy group_id")
}
