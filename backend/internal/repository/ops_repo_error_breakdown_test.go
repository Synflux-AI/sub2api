package repository

import (
	"strings"
	"testing"
)

func TestBuildErrorBreakdownQuery_UnknownDimensionRejected(t *testing.T) {
	if _, _, err := buildErrorBreakdownQuery("user_id; DROP TABLE", "WHERE 1=1", 2); err == nil {
		t.Fatalf("expected error for non-whitelisted dimension")
	}
}

func TestBuildErrorBreakdownQuery_UserDimensionJoinsAndLimits(t *testing.T) {
	items, totals, err := buildErrorBreakdownQuery("user", "WHERE created_at >= $1", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"FROM ops_error_logs",
		"WHERE created_at >= $1",
		"COALESCE(status_code, 0) >= 400",
		"GROUP BY 1",
		"LEFT JOIN users u ON u.id = g.k",
		"ORDER BY g.total DESC",
		"LIMIT $2",
	} {
		if !strings.Contains(items, want) {
			t.Errorf("items SQL missing %q\n%s", want, items)
		}
	}
	if strings.Contains(totals, "LIMIT") || strings.Contains(totals, "GROUP BY") {
		t.Errorf("totals SQL must be a single aggregate, got:\n%s", totals)
	}
	if !strings.Contains(totals, "FROM ops_error_logs") || !strings.Contains(totals, "WHERE created_at >= $1") {
		t.Errorf("totals SQL must reuse where:\n%s", totals)
	}
}

func TestBuildErrorBreakdownQuery_ValueDimensionNoJoin(t *testing.T) {
	items, _, err := buildErrorBreakdownQuery("status_code", "WHERE 1=1", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(items, "LEFT JOIN") {
		t.Errorf("status_code dim should not JOIN:\n%s", items)
	}
	if !strings.Contains(items, "COALESCE(upstream_status_code, status_code, 0)") {
		t.Errorf("status_code keyExpr missing:\n%s", items)
	}
}
