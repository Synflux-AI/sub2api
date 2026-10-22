package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 列类型必须与 usage_logs 同名列一致（046 / 070 / 029 / 136 / 224），下游按同一筛选值筛成功与错误。
func TestOpsErrorLogRequestAttributesMigration(t *testing.T) {
	content, err := FS.ReadFile("248_ops_error_log_request_attributes.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	for _, stmt := range []string{
		"ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS reasoning_effort VARCHAR(20);",
		"ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS service_tier VARCHAR(16);",
		"ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_size VARCHAR(10);",
		"ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_input_size VARCHAR(32);",
		"ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_response_format VARCHAR(16);",
		"ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_count INT;",
	} {
		require.Contains(t, sql, stmt)
	}
	require.Contains(t, sql, "SET LOCAL lock_timeout")
	require.NotContains(t, strings.ToUpper(sql), "DEFAULT ", "可空列不带默认值，NULL 表示未采集")
}
