package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 错误率虚线的分母（错误侧）只随实体过滤变，不随 error_owner/type/... 维度变。
// buildErrorEntityWhere 必须保留 时间+is_count_tokens+group/platform+实体(user/account/key/model)，
// 剥离 error_owner/error_source/error_type/error_phase/severity/status_codes。
func TestBuildErrorEntityWhere_KeepsEntityDropsErrorDims(t *testing.T) {
	uid := int64(7)
	acc := int64(3)
	ak := int64(9)
	gid := int64(5)
	filter := &service.OpsDashboardFilter{
		Platform:    "anthropic",
		GroupID:     &gid,
		UserID:      &uid,
		AccountID:   &acc,
		APIKeyID:    &ak,
		Model:       "claude-haiku-4-5",
		ErrorOwner:  "provider",
		ErrorSource: "network",
		ErrorType:   "api_error",
		ErrorPhase:  "upstream",
		Severity:    "P0",
		StatusCodes: []int{429, 529},
	}
	start := time.Unix(0, 0).UTC()
	end := time.Unix(3600, 0).UTC()

	where, args, next := buildErrorEntityWhere(filter, start, end, 1)

	for _, want := range []string{
		"created_at >= $",
		"created_at < $",
		"is_count_tokens = FALSE",
		"group_id = $",
		"platform = $",
		"user_id = $",
		"account_id = $",
		"api_key_id = $",
		"COALESCE(requested_model, model, '') = $",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("entity where missing %q\nfull: %s", want, where)
		}
	}
	for _, bad := range []string{
		"error_owner",
		"error_source",
		"error_type",
		"error_phase",
		"severity",
		"upstream_status_code, status_code, 0) IN",
	} {
		if strings.Contains(where, bad) {
			t.Errorf("entity where must NOT contain error-dim clause %q\nfull: %s", bad, where)
		}
	}
	if next <= 1 {
		t.Errorf("nextIndex should advance, got %d", next)
	}
	if len(args) == 0 {
		t.Errorf("expected some args, got none")
	}
}

// 分母的成功侧走 usage_logs。buildUsageWhere 只过滤 time+group+platform，缺实体，会让分母
// 不随 user/model 下钻缩小（率偏低）。buildUsageEntityWhere 必须补齐 user/account/key/model
// 实体过滤（ul. 前缀），且不受 error 维度影响（usage 表本无这些列）。
func TestBuildUsageEntityWhere_AddsEntityFilters(t *testing.T) {
	uid := int64(7)
	acc := int64(3)
	ak := int64(9)
	gid := int64(5)
	filter := &service.OpsDashboardFilter{
		Platform:   "anthropic",
		GroupID:    &gid,
		UserID:     &uid,
		AccountID:  &acc,
		APIKeyID:   &ak,
		Model:      "claude-haiku-4-5",
		ErrorOwner: "provider", // error 维度对 usage 侧应无影响
		ErrorType:  "api_error",
	}
	start := time.Unix(0, 0).UTC()
	end := time.Unix(3600, 0).UTC()

	join, where, args, next := buildUsageEntityWhere(filter, start, end, 1)

	for _, want := range []string{
		"ul.created_at >= $",
		"ul.created_at < $",
		"ul.group_id = $",
		"ul.user_id = $",
		"ul.account_id = $",
		"ul.api_key_id = $",
		"COALESCE(ul.requested_model, ul.model, '') = $",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("usage entity where missing %q\nfull: %s", want, where)
		}
	}
	// platform 过滤依赖 join groups/accounts。
	if !strings.Contains(join, "LEFT JOIN") {
		t.Errorf("platform filter should add join, got: %q", join)
	}
	for _, bad := range []string{"error_owner", "error_type"} {
		if strings.Contains(where, bad) {
			t.Errorf("usage where must NOT contain error-dim clause %q: %s", bad, where)
		}
	}
	if next <= 1 {
		t.Errorf("nextIndex should advance, got %d", next)
	}
	if len(args) == 0 {
		t.Errorf("expected some args, got none")
	}
}
