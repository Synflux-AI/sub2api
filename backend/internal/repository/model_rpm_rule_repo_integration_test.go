//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newModelRPMRuleRepoForTest(t *testing.T) (*modelRPMRuleRepository, context.Context) {
	t.Helper()
	return &modelRPMRuleRepository{sql: testTx(t)}, context.Background()
}

func insertModelRPMTestGroup(t *testing.T, repo *modelRPMRuleRepository, ctx context.Context, name string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, scanSingleRow(ctx, repo.sql,
		`INSERT INTO groups (name, platform) VALUES ($1, 'anthropic') RETURNING id`, []any{name}, &id))
	return id
}

func insertModelRPMTestUser(t *testing.T, repo *modelRPMRuleRepository, ctx context.Context, username, email string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, scanSingleRow(ctx, repo.sql,
		`INSERT INTO users (username, email, password_hash) VALUES ($1, $2, 'x') RETURNING id`,
		[]any{username, email}, &id))
	return id
}

func TestModelRPMRuleRepository_CRUDRoundTrip(t *testing.T) {
	repo, ctx := newModelRPMRuleRepoForTest(t)

	rule := &service.ModelRPMRule{
		Name:         "opus 全站池",
		ModelPattern: "claude-opus-*",
		Scope:        service.ModelRPMScopeGlobal,
		TargetType:   service.ModelRPMTargetAll,
		RPMLimit:     10,
		Enabled:      true,
	}
	require.NoError(t, repo.Create(ctx, rule))
	require.Positive(t, rule.ID)
	require.NotEmpty(t, rule.CreatedAt)

	got, err := repo.GetByID(ctx, rule.ID)
	require.NoError(t, err)
	require.Equal(t, "claude-opus-*", got.ModelPattern)
	require.Equal(t, service.ModelRPMScopeGlobal, got.Scope)
	require.Equal(t, service.ModelRPMTargetAll, got.TargetType)
	require.Nil(t, got.TargetID)
	require.Equal(t, 10, got.RPMLimit)
	require.True(t, got.Enabled)

	rule.RPMLimit = 25
	rule.Enabled = false
	rule.Name = "opus 全站池（停用）"
	require.NoError(t, repo.Update(ctx, rule))

	got, err = repo.GetByID(ctx, rule.ID)
	require.NoError(t, err)
	require.Equal(t, 25, got.RPMLimit)
	require.False(t, got.Enabled)
	require.Equal(t, "opus 全站池（停用）", got.Name)

	all, err := repo.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "ListAll 应含停用规则，由 resolver 负责过滤")

	require.NoError(t, repo.Delete(ctx, rule.ID))
	_, err = repo.GetByID(ctx, rule.ID)
	require.ErrorIs(t, err, service.ErrModelRPMRuleNotFound)
	require.ErrorIs(t, repo.Delete(ctx, rule.ID), service.ErrModelRPMRuleNotFound)
}

func TestModelRPMRuleRepository_ListResolvesTargetNames(t *testing.T) {
	repo, ctx := newModelRPMRuleRepoForTest(t)

	groupID := insertModelRPMTestGroup(t, repo, ctx, "rpm-rule-group")
	userID := insertModelRPMTestUser(t, repo, ctx, "rpm-rule-user", "rpm-rule-user@example.com")

	require.NoError(t, repo.Create(ctx, &service.ModelRPMRule{
		Name: "group rule", ModelPattern: "m1", Scope: service.ModelRPMScopeUser,
		TargetType: service.ModelRPMTargetGroup, TargetID: &groupID, RPMLimit: 5, Enabled: true,
	}))
	require.NoError(t, repo.Create(ctx, &service.ModelRPMRule{
		Name: "user rule", ModelPattern: "m2", Scope: service.ModelRPMScopeUser,
		TargetType: service.ModelRPMTargetUser, TargetID: &userID, RPMLimit: 5, Enabled: true,
	}))

	all, err := repo.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "rpm-rule-group", all[0].TargetName)
	require.Equal(t, "rpm-rule-user", all[1].TargetName)
}

func TestModelRPMRuleRepository_DuplicateEnabledRuleConflicts(t *testing.T) {
	base := func() *service.ModelRPMRule {
		return &service.ModelRPMRule{
			Name: "dup", ModelPattern: "claude-opus-*", Scope: service.ModelRPMScopeUser,
			TargetType: service.ModelRPMTargetAll, RPMLimit: 5, Enabled: true,
		}
	}

	t.Run("enabled duplicate rejected", func(t *testing.T) {
		repo, ctx := newModelRPMRuleRepoForTest(t)
		require.NoError(t, repo.Create(ctx, base()))
		require.ErrorIs(t, repo.Create(ctx, base()), service.ErrModelRPMRuleConflict,
			"同一 (模型, scope, target) 的启用规则应被 partial unique index 拦下")
	})

	// 唯一约束冲突会中止整个事务，停用规则的用例必须另开一个 tx。
	t.Run("disabled duplicate allowed", func(t *testing.T) {
		repo, ctx := newModelRPMRuleRepoForTest(t)
		require.NoError(t, repo.Create(ctx, base()))

		// partial index 只约束 enabled = TRUE，停用副本可以共存。
		disabled := base()
		disabled.Enabled = false
		require.NoError(t, repo.Create(ctx, disabled))

		another := base()
		another.Enabled = false
		require.NoError(t, repo.Create(ctx, another))
	})
}

func TestModelRPMRuleRepository_DatabaseRejectsInvalidRows(t *testing.T) {
	// 每个用例单独开事务：一条语句被 CHECK 拒绝后 Postgres 会中止整个事务，
	// 共用一个 tx 会让后续断言因「transaction is aborted」而假通过。
	cases := []struct {
		name string
		sql  string
	}{
		{
			// rpm_limit 必须为正：本表不提供 rpm_override=0 那样的免检语义。
			name: "rpm_limit must be positive",
			sql: `INSERT INTO model_rpm_rules (name, model_pattern, scope, target_type, rpm_limit)
				VALUES ('bad', 'm', 'user', 'all', 0)`,
		},
		{
			name: "group target requires target_id",
			sql: `INSERT INTO model_rpm_rules (name, model_pattern, scope, target_type, rpm_limit)
				VALUES ('bad', 'm', 'user', 'group', 5)`,
		},
		{
			name: "all target rejects target_id",
			sql: `INSERT INTO model_rpm_rules (name, model_pattern, scope, target_type, target_id, rpm_limit)
				VALUES ('bad', 'm', 'user', 'all', 1, 5)`,
		},
		{
			name: "scope value domain",
			sql: `INSERT INTO model_rpm_rules (name, model_pattern, scope, target_type, rpm_limit)
				VALUES ('bad', 'm', 'account', 'all', 5)`,
		},
		{
			name: "target_type value domain",
			sql: `INSERT INTO model_rpm_rules (name, model_pattern, scope, target_type, rpm_limit)
				VALUES ('bad', 'm', 'user', 'api_key', 5)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, ctx := newModelRPMRuleRepoForTest(t)
			_, err := repo.sql.ExecContext(ctx, tc.sql)
			require.Error(t, err)
		})
	}
}
