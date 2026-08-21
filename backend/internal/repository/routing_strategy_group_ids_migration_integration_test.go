//go:build integration

package repository

import (
	"context"
	"testing"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

// TestMigration229BackfillsRoutingStrategyGroupIds 验证迁移 229 把
// routing_strategies.group_id 单值列正确回填为 group_ids 数组列，且重复执行幂等：
//   - group_id 非空的行回填为单元素数组
//   - group_id 为 NULL 的行保持 group_ids 为空数组
//   - 已有非空 group_ids（尤其是多元素数组）的行不会被重复回填覆盖
func TestMigration229BackfillsRoutingStrategyGroupIds(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	migrationSQL, err := dbmigrations.FS.ReadFile("229_routing_strategy_group_ids.sql")
	require.NoError(t, err)

	// 行 A：存量单值列，group_ids 尚未回填（保持迁移前的默认空数组），期望回填为 [group_id]。
	var singleValueID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO routing_strategies (name, group_id)
VALUES ('migration-229-single-value', 42)
RETURNING id
`).Scan(&singleValueID))

	// 行 B：group_id 为 NULL，期望 group_ids 保持空数组。
	var nullGroupID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO routing_strategies (name, group_id)
VALUES ('migration-229-null-group', NULL)
RETURNING id
`).Scan(&nullGroupID))

	// 行 C：模拟已完成多选迁移的行——group_ids 是多元素数组，group_id 按不变量等于 group_ids[0]。
	// 迁移的回填条件是 group_ids = '[]'::jsonb，所以这一行不应被本迁移的 UPDATE 触碰。
	var multiValueID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO routing_strategies (name, group_id, group_ids)
VALUES ('migration-229-multi-value', 7, '[7,8,9]'::jsonb)
RETURNING id
`).Scan(&multiValueID))

	// 第一次执行：完成回填。
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)

	assertRoutingStrategyGroupIDs := func(id int64, want string) {
		var groupIDs string
		require.NoError(t, tx.QueryRowContext(ctx, `
SELECT group_ids::text FROM routing_strategies WHERE id = $1
`, id).Scan(&groupIDs))
		require.JSONEq(t, want, groupIDs)
	}

	assertRoutingStrategyGroupIDs(singleValueID, `[42]`)
	assertRoutingStrategyGroupIDs(nullGroupID, `[]`)
	assertRoutingStrategyGroupIDs(multiValueID, `[7,8,9]`)

	// 第二次执行：验证幂等。已回填的行不应被再次改写，尤其不能把多元素数组压回单元素。
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)

	assertRoutingStrategyGroupIDs(singleValueID, `[42]`)
	assertRoutingStrategyGroupIDs(nullGroupID, `[]`)
	assertRoutingStrategyGroupIDs(multiValueID, `[7,8,9]`)
}
