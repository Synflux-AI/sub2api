package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 搜索子句必须覆盖 upstream_error_message，否则「流中断」类错误的真实 cause
// 写进该列后，后台按 "missing terminal event" 仍然搜不到。
// 必须是完整的 5 列（4 个原有列逐字节等价 + 新增 upstream_error_message），
// 以及原有顺序、正确的括号和 OR 连接。
func TestOpsErrorLogSearchClauseCoversUpstreamErrorMessage(t *testing.T) {
	clause := opsErrorLogSearchClause("$1")

	// 断言完整的期望 SQL 片段（5 列、原有顺序、upstream_error_message 在最后、括号、OR 连接）
	expected := "(e.request_id ILIKE $1 OR e.client_request_id ILIKE $1 OR e.trace_id ILIKE $1 OR e.error_message ILIKE $1 OR e.upstream_error_message ILIKE $1)"
	require.Equal(t, expected, clause)

	// 额外断言：upstream_error_message 必须存在（说明新列的意图）
	require.Contains(t, clause, "e.upstream_error_message ILIKE $1")
}
