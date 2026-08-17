package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 搜索子句必须覆盖 upstream_error_message，否则「流中断」类错误的真实 cause
// 写进该列后，后台按 "missing terminal event" 仍然搜不到。
func TestOpsErrorLogSearchClauseCoversUpstreamErrorMessage(t *testing.T) {
	clause := opsErrorLogSearchClause("$1")
	require.Contains(t, clause, "e.error_message ILIKE $1")
	require.Contains(t, clause, "e.upstream_error_message ILIKE $1")
	require.True(t, strings.HasPrefix(clause, "("))
	require.True(t, strings.HasSuffix(clause, ")"))
}
