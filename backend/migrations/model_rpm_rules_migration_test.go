package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelRPMRulesMigrationCreatesRuleTable(t *testing.T) {
	content, err := FS.ReadFile("234_model_rpm_rules.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS model_rpm_rules")
	require.Contains(t, sql, "model_pattern TEXT NOT NULL")
	require.Contains(t, sql, "scope TEXT NOT NULL")
	require.Contains(t, sql, "target_type TEXT NOT NULL")
	require.Contains(t, sql, "target_id BIGINT")
	require.Contains(t, sql, "rpm_limit INTEGER NOT NULL")
	require.Contains(t, sql, "enabled BOOLEAN NOT NULL DEFAULT TRUE")

	// 取值域与「rpm_limit 必须为正」由数据库兜底，不只靠 service 校验。
	require.Contains(t, sql, "CHECK (scope IN ('user', 'global'))")
	require.Contains(t, sql, "CHECK (target_type IN ('all', 'group', 'user'))")
	require.Contains(t, sql, "CHECK (rpm_limit > 0)")
	require.Contains(t, sql, "(target_type = 'all' AND target_id IS NULL)")

	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_model_rpm_rules_enabled ON model_rpm_rules(enabled)")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_model_rpm_rules_target ON model_rpm_rules(target_type, target_id)")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_model_rpm_rules_enabled_unique")
	require.Contains(t, sql, "COALESCE(target_id, 0)) WHERE enabled = TRUE")

	// 迁移只新建独立表，不触碰既有 RPM 载体。
	require.NotContains(t, strings.ToUpper(sql), "ALTER TABLE ")
	require.NotContains(t, strings.ToUpper(sql), "DROP ")
}

func TestModelRPMRulesMigrationNumberIsUnused(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	var owners []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "234_") {
			owners = append(owners, entry.Name())
		}
	}
	require.Equal(t, []string{"234_model_rpm_rules.sql"}, owners,
		"迁移号 234 应只属于本迁移；撞号会让 schema_migrations 记录彼此覆盖")
}
