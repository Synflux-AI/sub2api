package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestBuildErrorWhere_AppliesNewDimensionFilters(t *testing.T) {
	uid := int64(7)
	acc := int64(3)
	ak := int64(9)
	filter := &service.OpsDashboardFilter{
		UserID:      &uid,
		AccountID:   &acc,
		APIKeyID:    &ak,
		Model:       "claude-haiku-4-5",
		ErrorOwner:  "Provider", // 大写，断言归一为小写参数
		ErrorSource: "Network",
		ErrorType:   "api_error",
		ErrorPhase:  "Upstream", // 大写，断言归一为小写参数
		Severity:    "P0",
		StatusCodes: []int{429, 529},
	}
	start := time.Unix(0, 0).UTC()
	end := time.Unix(3600, 0).UTC()

	where, args, next := buildErrorWhere(filter, start, end, 1)

	for _, want := range []string{
		"user_id = $",
		"account_id = $",
		"api_key_id = $",
		"COALESCE(requested_model, model, '') = $",
		"LOWER(COALESCE(error_owner,'')) = $",
		"LOWER(COALESCE(error_source,'')) = $",
		"error_type = $",
		"error_phase = $",
		"severity = $",
		"COALESCE(upstream_status_code, status_code, 0) IN ($",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("where missing %q\nfull: %s", want, where)
		}
	}
	if strings.Contains(where, "e.user_id") {
		t.Errorf("where must stay unaliased, got: %s", where)
	}
	foundProvider := false
	foundPhase := false
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			continue
		}
		switch s {
		case "provider":
			foundProvider = true
		case "upstream":
			foundPhase = true
		case "Provider", "Upstream":
			t.Errorf("owner/phase arg should be lowercased, got %q", s)
		}
	}
	if !foundProvider {
		t.Errorf("expected lowercased 'provider' in args: %#v", args)
	}
	if !foundPhase {
		t.Errorf("expected lowercased 'upstream' (phase) in args: %#v", args)
	}
	if next <= 1 {
		t.Errorf("nextIndex should advance, got %d", next)
	}
}

func TestBuildOpsErrorLogsWhere_QueryUsesQualifiedColumns(t *testing.T) {
	filter := &service.OpsErrorLogFilter{
		Query: "ACCESS_DENIED",
	}

	where, args := buildOpsErrorLogsWhere(filter)
	if where == "" {
		t.Fatalf("where should not be empty")
	}
	if len(args) != 1 {
		t.Fatalf("args len = %d, want 1", len(args))
	}
	if !strings.Contains(where, "e.request_id ILIKE $") {
		t.Fatalf("where should include qualified request_id condition: %s", where)
	}
	if !strings.Contains(where, "e.client_request_id ILIKE $") {
		t.Fatalf("where should include qualified client_request_id condition: %s", where)
	}
	if !strings.Contains(where, "e.error_message ILIKE $") {
		t.Fatalf("where should include qualified error_message condition: %s", where)
	}
}

func TestBuildOpsErrorLogsWhere_UserQueryUsesExistsSubquery(t *testing.T) {
	filter := &service.OpsErrorLogFilter{
		UserQuery: "admin@",
	}

	where, args := buildOpsErrorLogsWhere(filter)
	if where == "" {
		t.Fatalf("where should not be empty")
	}
	if len(args) != 1 {
		t.Fatalf("args len = %d, want 1", len(args))
	}
	if !strings.Contains(where, "EXISTS (SELECT 1 FROM users u WHERE u.id = e.user_id AND u.email ILIKE $") {
		t.Fatalf("where should include EXISTS user email condition: %s", where)
	}
}
