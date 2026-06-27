package repository

import (
	"strings"
	"testing"
)

func TestBuildErrorTrendByDimQuery_UnknownDimensionRejected(t *testing.T) {
	if _, err := buildErrorTrendByDimQuery("user_id; DROP TABLE", "WHERE 1=1", "date_trunc('hour', created_at)", 2); err == nil {
		t.Fatal("expected error for unknown dimension")
	}
}

func TestBuildErrorTrendByDimQuery_UserDimensionJoinsAndOthers(t *testing.T) {
	q, err := buildErrorTrendByDimQuery("user", "WHERE created_at >= $1", "date_trunc('hour', created_at)", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"LEFT JOIN users u", "__others__", "FILTER (WHERE", "LIMIT $2", "GROUP BY"} {
		if !strings.Contains(q, frag) {
			t.Fatalf("query missing %q:\n%s", frag, q)
		}
	}
}

func TestBuildErrorTrendByDimQuery_StatusCodeNoJoin(t *testing.T) {
	q, err := buildErrorTrendByDimQuery("status_code", "WHERE 1=1", "date_trunc('hour', created_at)", 2)
	if err != nil {
		t.Fatal(err)
	}
	// status_code 无 label 维度表 JOIN：labeled CTE 的 FROM 应是裸 topn g（不接 users/accounts 等）。
	// 注意：最外层 agg LEFT JOIN labeled 是固定的 label 关联结构，与维度无关，故不能直接断言无 "LEFT JOIN"。
	if !strings.Contains(q, "FROM topn g \n") {
		t.Fatalf("status_code labeled CTE should be bare topn (no dim join):\n%s", q)
	}
	for _, j := range []string{"LEFT JOIN users", "LEFT JOIN accounts", "LEFT JOIN api_keys", "LEFT JOIN groups"} {
		if strings.Contains(q, j) {
			t.Fatalf("status_code should not contain %q:\n%s", j, q)
		}
	}
}
