package repository

import (
	"strings"
	"testing"
)

// request_total 分母 = 成功(usage_logs) + 错误(ops_error_logs status>=400)，逐桶聚合。
// 组装的 SQL 必须：用调用方传入的 entity where、错误侧显式 status>=400、两侧 UNION ALL 后按桶求和。
func TestBuildRequestTotalsQuery_UsesBothSidesAndStatusFilter(t *testing.T) {
	q := buildRequestTotalsQuery(
		"LEFT JOIN groups g ON g.id = ul.group_id",
		"WHERE ul.created_at >= $1 AND ul.user_id = $2",
		"WHERE created_at >= $3 AND user_id = $4",
		"date_trunc('minute', ul.created_at)",
		"date_trunc('minute', created_at)",
	)
	for _, frag := range []string{
		"FROM usage_logs ul",
		"LEFT JOIN groups g ON g.id = ul.group_id",
		"FROM ops_error_logs",
		"COALESCE(status_code, 0) >= 400",
		"UNION ALL",
		"AS request_total",
		"ORDER BY bucket ASC",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("request_totals query missing %q:\n%s", frag, q)
		}
	}
}
