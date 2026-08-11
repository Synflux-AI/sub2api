package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpsSystemLogTraceIDIndexMigration(t *testing.T) {
	content, err := FS.ReadFile("221_add_ops_system_logs_trace_id_index_notx.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_system_logs_trace_id")
	require.Contains(t, sql, "ON ops_system_logs ((extra ->> 'trace_id'))")
	require.Contains(t, sql, "WHERE extra ->> 'trace_id' IS NOT NULL")
}
