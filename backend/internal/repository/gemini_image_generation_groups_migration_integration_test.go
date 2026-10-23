//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestMigration246EnablesOnlyGeminiImageGroups(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	migration, err := dbmigrations.FS.ReadFile("246_enable_gemini_image_generation_groups.sql")
	require.NoError(t, err)

	for _, platform := range []string{"gemini", "antigravity", "composite", "openai", "anthropic"} {
		for _, enabled := range []bool{false, true} {
			var id int64
			require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO groups (name, platform, rate_multiplier, status, allow_image_generation)
VALUES ($1, $2, 1, 'active', $3) RETURNING id
`, fmt.Sprintf("migration-246-%s-%t", platform, enabled), platform, enabled).Scan(&id))
			// 同一事务内重放，验证存量开关的更新范围与幂等性。
			for range 2 {
				_, err := tx.ExecContext(ctx, string(migration))
				require.NoError(t, err)
				var got bool
				require.NoError(t, tx.QueryRowContext(ctx,
					"SELECT allow_image_generation FROM groups WHERE id = $1", id).Scan(&got))
				require.Equal(t, enabled || platform == "gemini" || platform == "antigravity", got,
					"platform=%s, previous=%t", platform, enabled)
			}
		}
	}
}
