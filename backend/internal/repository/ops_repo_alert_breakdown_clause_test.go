package repository

import (
	"strings"
	"testing"
)

func TestAlertBreakdownErrorPredicate_Upstream(t *testing.T) {
	got := alertBreakdownErrorPredicate("upstream_error_rate", "")
	for _, want := range []string{
		"error_owner='provider'",
		"NOT is_business_limited",
		"COALESCE(upstream_status_code,status_code,0) NOT IN (429,529)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("upstream predicate missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "status_code,0)>=400") || strings.Contains(got, "status_code,0) >= 400") {
		t.Fatalf("upstream predicate must NOT require status>=400: %s", got)
	}
	if strings.Contains(got, "COALESCE(is_business_limited") {
		t.Fatalf("upstream predicate must use bare NOT is_business_limited: %s", got)
	}
}

func TestAlertBreakdownErrorPredicate_DefaultKeepsStatus400(t *testing.T) {
	for _, mt := range []string{"error_rate", "success_rate", ""} {
		got := alertBreakdownErrorPredicate(mt, "")
		if !strings.Contains(got, "COALESCE(status_code,0)>=400") {
			t.Fatalf("metric %q must keep status>=400: %s", mt, got)
		}
		if !strings.Contains(got, "COALESCE(is_business_limited,false)=false") {
			t.Fatalf("metric %q must keep business_limited filter: %s", mt, got)
		}
	}
}

func TestAlertBreakdownErrorPredicate_Prefix(t *testing.T) {
	got := alertBreakdownErrorPredicate("upstream_error_rate", "e.")
	if !strings.Contains(got, "e.error_owner='provider'") || !strings.Contains(got, "e.upstream_status_code") {
		t.Fatalf("prefix not applied: %s", got)
	}
}

func TestAlertBreakdownStatusExpr(t *testing.T) {
	if got := alertBreakdownStatusExpr("upstream_error_rate", ""); got != "COALESCE(upstream_status_code,status_code,0)" {
		t.Fatalf("upstream status expr = %s", got)
	}
	if got := alertBreakdownStatusExpr("error_rate", "e."); got != "COALESCE(e.status_code,0)" {
		t.Fatalf("default status expr = %s", got)
	}
}

func TestAlertBreakdownSLAPredicate(t *testing.T) {
	got := alertBreakdownSLAPredicate("")
	if !strings.Contains(got, "COALESCE(status_code,0)>=400") || !strings.Contains(got, "NOT is_business_limited") {
		t.Fatalf("sla predicate = %s", got)
	}
}
