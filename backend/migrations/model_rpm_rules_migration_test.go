package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelRPMRulesMigrationCreatesRuleTable(t *testing.T) {
	content, err := FS.ReadFile("238_model_rpm_rules.sql")
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
		if strings.HasPrefix(entry.Name(), "238_") {
			owners = append(owners, entry.Name())
		}
	}
	// schema_migrations 以 filename 为主键（见 migrations_runner.go 的 DDL），
	// 同号不同名的迁移各占一行、按文件名排序执行，不会彼此覆盖。上游也用了 238，
	// 这里只守住本迁移文件本身没被改名或删除。
	require.Contains(t, owners, "238_model_rpm_rules.sql",
		"本迁移文件不应被改名或删除；改名会让已应用过的实例重跑一次")
}
