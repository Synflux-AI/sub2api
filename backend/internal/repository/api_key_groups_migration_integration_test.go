//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const migration230File = "230_add_api_key_groups.sql"

// TestMigration230CreatesAPIKeyGroupsSchema 验证迁移 230 建出来的 api_key_groups 结构：
// 列、复合主键 (api_key_id, group_id)、唯一索引 (api_key_id, platform)、group_id 反向索引。
func TestMigration230CreatesAPIKeyGroupsSchema(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	// 迁移已在 TestMain 的 ApplyMigrations 中执行，这里直接断言真实库上的结构。
	var tableExists bool
	require.NoError(t, tx.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
     WHERE table_schema = current_schema() AND table_name = 'api_key_groups'
)
`).Scan(&tableExists))
	require.True(t, tableExists, "api_key_groups 表应已由迁移 230 创建")

	columns := map[string]string{}
	rows, err := tx.QueryContext(ctx, `
SELECT column_name, is_nullable
  FROM information_schema.columns
 WHERE table_schema = current_schema() AND table_name = 'api_key_groups'
`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, nullable string
		require.NoError(t, rows.Scan(&name, &nullable))
		columns[name] = nullable
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]string{
		"api_key_id": "NO",
		"group_id":   "NO",
		"platform":   "NO",
		"created_at": "NO",
	}, columns)

	// 复合主键 (api_key_id, group_id) —— 保证「同一个 Key 不能重复绑定同一个分组」。
	var pkColumns string
	require.NoError(t, tx.QueryRowContext(ctx, `
SELECT string_agg(a.attname, ',' ORDER BY k.ord)
  FROM pg_constraint c
  CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
 WHERE c.conrelid = 'api_key_groups'::regclass AND c.contype = 'p'
`).Scan(&pkColumns))
	require.Equal(t, "api_key_id,group_id", pkColumns)

	// (api_key_id, platform) 唯一索引 + group_id 反向索引。
	assertIndex := func(name string, wantUnique bool, wantColumns string) {
		t.Helper()
		var isUnique bool
		var indexColumns string
		require.NoError(t, tx.QueryRowContext(ctx, `
SELECT i.indisunique,
       string_agg(a.attname, ',' ORDER BY k.ord)
  FROM pg_index i
  JOIN pg_class ic ON ic.oid = i.indexrelid
  CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
 WHERE i.indrelid = 'api_key_groups'::regclass AND ic.relname = $1
 GROUP BY i.indisunique
`, name).Scan(&isUnique, &indexColumns), "索引 %s 应存在", name)
		require.Equal(t, wantUnique, isUnique, "索引 %s 的唯一性", name)
		require.Equal(t, wantColumns, indexColumns, "索引 %s 的列", name)
	}
	assertIndex("idx_api_key_groups_key_platform", true, "api_key_id,platform")
	assertIndex("idx_api_key_groups_group_id", false, "group_id")
}

// TestMigration230APIKeyGroupsConstraintsAreEnforced 验证两个唯一约束在真实库上生效：
//   - (api_key_id, group_id)：同一个 Key 不能重复绑定同一个分组
//   - (api_key_id, platform)：同一个 Key 在同一平台下只能绑定一个分组
//
// 同时验证「同一个 Key 绑定不同平台的多个分组」是被允许的（本 issue 的核心场景）。
func TestMigration230APIKeyGroupsConstraintsAreEnforced(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	fx := insertMigration230Fixtures(t, ctx, tx, "constraints")

	_, err := tx.ExecContext(ctx, `
INSERT INTO api_key_groups (api_key_id, group_id, platform) VALUES ($1, $2, 'anthropic')
`, fx.boundKeyID, fx.anthropicGroupID)
	require.NoError(t, err)

	// 同一个 Key + 同一个分组 -> 复合主键冲突。
	requireMigration230UniqueViolation(t, ctx, tx, "dup_key_group", "api_key_groups_pkey", `
INSERT INTO api_key_groups (api_key_id, group_id, platform) VALUES ($1, $2, 'anthropic')
`, fx.boundKeyID, fx.anthropicGroupID)

	// 同一个 Key + 不同分组，但 platform 相同 -> (api_key_id, platform) 唯一索引冲突。
	requireMigration230UniqueViolation(t, ctx, tx, "dup_key_platform", "idx_api_key_groups_key_platform", `
INSERT INTO api_key_groups (api_key_id, group_id, platform) VALUES ($1, $2, 'anthropic')
`, fx.boundKeyID, fx.openaiGroupID)

	// 同一个 Key + 不同分组 + 不同 platform -> 允许，这正是多分组绑定要支持的形态。
	_, err = tx.ExecContext(ctx, `
INSERT INTO api_key_groups (api_key_id, group_id, platform) VALUES ($1, $2, 'openai')
`, fx.boundKeyID, fx.openaiGroupID)
	require.NoError(t, err)

	// 不同 Key 绑定同一个分组 -> 允许。
	_, err = tx.ExecContext(ctx, `
INSERT INTO api_key_groups (api_key_id, group_id, platform) VALUES ($1, $2, 'anthropic')
`, fx.unboundKeyID, fx.anthropicGroupID)
	require.NoError(t, err)
}

// TestMigration230BackfillsAPIKeyGroupsFromGroupID 验证迁移 230 把存量
// api_keys.group_id 正确回填成 api_key_groups 关联行，且重复执行幂等：
//   - group_id 非空且未软删的 Key 产生一行，platform 取自 groups.platform
//   - group_id 为 NULL 的 Key 不产生行
//   - 已软删的 Key 不产生行
//   - 重复执行不报错、不改变结果，也不会覆盖迁移后新增的绑定行
func TestMigration230BackfillsAPIKeyGroupsFromGroupID(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	migrationSQL, err := dbmigrations.FS.ReadFile(migration230File)
	require.NoError(t, err)

	fx := insertMigration230Fixtures(t, ctx, tx, "backfill")

	// 迁移已在 TestMain 中跑过，这里的夹具行是「迁移之后新插入的存量数据」，
	// 重跑整份迁移即可复现回填行为（CREATE TABLE / CREATE INDEX 都是 IF NOT EXISTS，天然幂等）。
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)

	assertBindings := func(stage string) {
		t.Helper()

		// group_id 非空、未软删 -> 回填一行，platform 来自 groups.platform。
		bindings := migration230Bindings(t, ctx, tx, fx.boundKeyID)
		require.Equal(t, [][2]any{{fx.anthropicGroupID, "anthropic"}}, bindings, "%s: 存量绑定应被回填", stage)

		// group_id 为 NULL -> 不产生行。
		require.Empty(t, migration230Bindings(t, ctx, tx, fx.unboundKeyID), "%s: group_id 为 NULL 不应产生绑定", stage)

		// 已软删的 Key -> 不产生行。
		require.Empty(t, migration230Bindings(t, ctx, tx, fx.deletedKeyID), "%s: 已软删的 Key 不应产生绑定", stage)

		// 平台快照必须取自 groups.platform，不能是硬编码默认值。
		bindings = migration230Bindings(t, ctx, tx, fx.openaiKeyID)
		require.Equal(t, [][2]any{{fx.openaiGroupID, "openai"}}, bindings, "%s: platform 应取自 groups.platform", stage)
	}
	assertBindings("首次执行")

	// 迁移之后由应用层新增的跨平台绑定行：重跑迁移不能删除或改写它。
	_, err = tx.ExecContext(ctx, `
INSERT INTO api_key_groups (api_key_id, group_id, platform) VALUES ($1, $2, 'openai')
`, fx.boundKeyID, fx.openaiGroupID)
	require.NoError(t, err)

	// 第二次执行：验证幂等。
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)

	require.Equal(t, [][2]any{
		{fx.anthropicGroupID, "anthropic"},
		{fx.openaiGroupID, "openai"},
	}, migration230Bindings(t, ctx, tx, fx.boundKeyID), "重跑迁移不应改动已有绑定行")

	require.Empty(t, migration230Bindings(t, ctx, tx, fx.unboundKeyID))
	require.Empty(t, migration230Bindings(t, ctx, tx, fx.deletedKeyID))
	require.Equal(t, [][2]any{{fx.openaiGroupID, "openai"}}, migration230Bindings(t, ctx, tx, fx.openaiKeyID))
}

type migration230Fixtures struct {
	userID           int64
	anthropicGroupID int64
	openaiGroupID    int64
	boundKeyID       int64
	openaiKeyID      int64
	unboundKeyID     int64
	deletedKeyID     int64
}

// insertMigration230Fixtures 在测试事务内造出回填/约束断言需要的边界数据。
// suffix 用于隔离不同用例的唯一列（users.email、groups.name、api_keys.key）。
func insertMigration230Fixtures(t *testing.T, ctx context.Context, tx *sql.Tx, suffix string) migration230Fixtures {
	t.Helper()

	var fx migration230Fixtures
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id
`, fmt.Sprintf("migration-230-%s@example.test", suffix)).Scan(&fx.userID))

	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO groups (name, platform) VALUES ($1, 'anthropic') RETURNING id
`, fmt.Sprintf("migration-230-%s-anthropic", suffix)).Scan(&fx.anthropicGroupID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO groups (name, platform) VALUES ($1, 'openai') RETURNING id
`, fmt.Sprintf("migration-230-%s-openai", suffix)).Scan(&fx.openaiGroupID))

	insertKey := func(name string, groupID *int64, softDeleted bool) int64 {
		t.Helper()
		var id int64
		require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO api_keys (user_id, key, name, group_id, deleted_at)
VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN NOW() ELSE NULL END)
RETURNING id
`, fx.userID, fmt.Sprintf("sk-migration-230-%s-%s", suffix, name), name, groupID, softDeleted).Scan(&id))
		return id
	}

	fx.boundKeyID = insertKey("bound", &fx.anthropicGroupID, false)
	fx.openaiKeyID = insertKey("openai", &fx.openaiGroupID, false)
	fx.unboundKeyID = insertKey("unbound", nil, false)
	fx.deletedKeyID = insertKey("deleted", &fx.openaiGroupID, true)

	return fx
}

// migration230Bindings 返回指定 Key 的 (group_id, platform) 绑定列表，按 group_id 排序。
func migration230Bindings(t *testing.T, ctx context.Context, tx *sql.Tx, apiKeyID int64) [][2]any {
	t.Helper()

	rows, err := tx.QueryContext(ctx, `
SELECT group_id, platform FROM api_key_groups WHERE api_key_id = $1 ORDER BY group_id
`, apiKeyID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := [][2]any{}
	for rows.Next() {
		var groupID int64
		var platform string
		require.NoError(t, rows.Scan(&groupID, &platform))
		out = append(out, [2]any{groupID, platform})
	}
	require.NoError(t, rows.Err())
	return out
}

// requireMigration230UniqueViolation 在 SAVEPOINT 内执行 stmt，断言它触发指定约束的唯一性冲突，
// 然后回滚到 SAVEPOINT，让外层测试事务继续可用。
func requireMigration230UniqueViolation(
	t *testing.T,
	ctx context.Context,
	tx *sql.Tx,
	savepoint string,
	wantConstraint string,
	stmt string,
	args ...any,
) {
	t.Helper()

	_, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, stmt, args...)
	require.Error(t, err, "期望违反约束 %s", wantConstraint)
	var pqErr *pq.Error
	require.ErrorAs(t, err, &pqErr)
	require.Equal(t, pq.ErrorCode("23505"), pqErr.Code, "期望 unique_violation")
	require.Equal(t, wantConstraint, pqErr.Constraint)

	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint)
	require.NoError(t, err)
}
