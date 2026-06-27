package admin

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newTestCtx(rawQuery string) *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x?"+rawQuery, nil)
	return c
}

func TestParseOpsDashboardErrorFilter_ParsesDimensions(t *testing.T) {
	start := time.Unix(0, 0).UTC()
	end := time.Unix(3600, 0).UTC()
	c := newTestCtx("platform=anthropic&group_id=2&user_id=7&account_id=3&api_key_id=9&model=claude-haiku-4-5&error_owner=provider&error_source=network&error_type=api_error&phase=upstream&severity=P0&status_codes=429,529")

	f, err := parseOpsDashboardErrorFilter(c, start, end)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.UserID == nil || *f.UserID != 7 {
		t.Errorf("user_id not parsed: %#v", f.UserID)
	}
	if f.AccountID == nil || *f.AccountID != 3 {
		t.Errorf("account_id not parsed")
	}
	if f.APIKeyID == nil || *f.APIKeyID != 9 {
		t.Errorf("api_key_id not parsed")
	}
	if f.Model != "claude-haiku-4-5" || f.ErrorOwner != "provider" || f.ErrorType != "api_error" || f.ErrorPhase != "upstream" || f.Severity != "P0" {
		t.Errorf("string dims not parsed: %#v", f)
	}
	if len(f.StatusCodes) != 2 || f.StatusCodes[0] != 429 || f.StatusCodes[1] != 529 {
		t.Errorf("status_codes not parsed: %#v", f.StatusCodes)
	}
}

func TestParseOpsDashboardErrorFilter_RejectsBadUserID(t *testing.T) {
	c := newTestCtx("user_id=abc")
	if _, err := parseOpsDashboardErrorFilter(c, time.Unix(0, 0), time.Unix(1, 0)); err == nil {
		t.Errorf("expected error for invalid user_id")
	}
}

func TestParseOpsBreakdownLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 20, false},
		{"50", 50, false},
		{"0", 0, true},
		{"101", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := parseOpsBreakdownLimit(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("raw=%q expected error", tc.raw)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("raw=%q got (%d,%v) want %d", tc.raw, got, err, tc.want)
		}
	}
}
