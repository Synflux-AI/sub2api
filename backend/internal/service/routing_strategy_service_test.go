package service

import (
	"context"
	"testing"
)

type stubRoutingStrategyRepo struct {
	enabled []RoutingStrategy
}

func (r *stubRoutingStrategyRepo) Create(context.Context, *RoutingStrategy) error { return nil }
func (r *stubRoutingStrategyRepo) GetByID(context.Context, int64) (*RoutingStrategy, error) {
	return nil, ErrRoutingStrategyNotFound
}
func (r *stubRoutingStrategyRepo) Update(context.Context, *RoutingStrategy) error { return nil }
func (r *stubRoutingStrategyRepo) Delete(context.Context, int64) error            { return nil }
func (r *stubRoutingStrategyRepo) List(context.Context) ([]RoutingStrategy, error) {
	return r.enabled, nil
}
func (r *stubRoutingStrategyRepo) ListEnabled(context.Context) ([]RoutingStrategy, error) {
	return r.enabled, nil
}

func newTestRoutingService(strategies ...RoutingStrategy) *RoutingStrategyService {
	return NewRoutingStrategyService(&stubRoutingStrategyRepo{enabled: strategies})
}

// ptrInt64 must live in this untagged test file so it is available to both the
// default build (golangci-lint typecheck) and the `unit`-tagged build. The
// payment_config_plans_validation_test.go helpers are gated behind //go:build
// unit, so they cannot provide it for the default build. Do not move/remove.
func ptrInt64(v int64) *int64 { return &v }

func TestRoutingEvaluate_ModelWildcardRestrict(t *testing.T) {
	svc := newTestRoutingService(RoutingStrategy{
		ID: 1, Name: "opus->A", Enabled: true, Priority: 10,
		Platform: PlatformAnthropic, Action: RoutingActionRestrict, MatchMode: RoutingMatchModeAll,
		Conditions: []RoutingCondition{{Type: RoutingConditionTypeModel, Op: RoutingConditionOpWildcard, Value: "claude-opus-*"}},
		AccountIDs: []int64{101, 102},
	})

	dec := svc.Evaluate(context.Background(), RoutingMatchContext{
		Platform: PlatformAnthropic, Model: "claude-opus-4-20250514", ClientType: RoutingClientOther,
	})
	if !dec.HasMatch() || dec.MatchedID != 1 {
		t.Fatalf("expected match strategy 1, got %+v", dec)
	}
	if len(dec.RestrictIDs) != 2 || len(dec.PreferIDs) != 0 {
		t.Fatalf("expected restrict ids, got %+v", dec)
	}

	// 不匹配的模型
	dec = svc.Evaluate(context.Background(), RoutingMatchContext{
		Platform: PlatformAnthropic, Model: "claude-sonnet-4", ClientType: RoutingClientOther,
	})
	if dec.HasMatch() {
		t.Fatalf("expected no match, got %+v", dec)
	}
}

func TestRoutingEvaluate_ClientPrefer(t *testing.T) {
	svc := newTestRoutingService(RoutingStrategy{
		ID: 2, Name: "cc->B", Enabled: true, Priority: 10,
		Platform: PlatformAnthropic, Action: RoutingActionPrefer, MatchMode: RoutingMatchModeAll,
		Conditions: []RoutingCondition{{Type: RoutingConditionTypeClient, Value: RoutingClientClaudeCode}},
		AccountIDs: []int64{201},
	})

	dec := svc.Evaluate(context.Background(), RoutingMatchContext{
		Platform: PlatformAnthropic, Model: "claude-sonnet-4", ClientType: RoutingClientClaudeCode,
	})
	if !dec.HasMatch() || len(dec.PreferIDs) != 1 || dec.PreferIDs[0] != 201 {
		t.Fatalf("expected prefer 201, got %+v", dec)
	}
	if len(dec.RestrictIDs) != 0 {
		t.Fatalf("prefer should not set restrict, got %+v", dec)
	}

	// 非 claude code 客户端不命中
	dec = svc.Evaluate(context.Background(), RoutingMatchContext{
		Platform: PlatformAnthropic, Model: "claude-sonnet-4", ClientType: RoutingClientOther,
	})
	if dec.HasMatch() {
		t.Fatalf("expected no match for non-cc client, got %+v", dec)
	}
}

func TestRoutingEvaluate_UserAgentContainsAndRegex(t *testing.T) {
	svc := newTestRoutingService(
		RoutingStrategy{
			ID: 3, Name: "ua-contains", Enabled: true, Priority: 10, Platform: PlatformAnthropic,
			Action: RoutingActionRestrict, MatchMode: RoutingMatchModeAll,
			Conditions: []RoutingCondition{{Type: RoutingConditionTypeUserAgent, Op: RoutingConditionOpContains, Value: "claude-cli"}},
			AccountIDs: []int64{301},
		},
	)
	dec := svc.Evaluate(context.Background(), RoutingMatchContext{
		Platform: PlatformAnthropic, Model: "x", ClientType: RoutingClientOther, UserAgent: "claude-cli/2.1.0 (external)",
	})
	if !dec.HasMatch() || dec.RestrictIDs[0] != 301 {
		t.Fatalf("expected ua contains match, got %+v", dec)
	}

	svc = newTestRoutingService(RoutingStrategy{
		ID: 4, Name: "ua-regex", Enabled: true, Priority: 10, Platform: PlatformAnthropic,
		Action: RoutingActionRestrict, MatchMode: RoutingMatchModeAll,
		Conditions: []RoutingCondition{{Type: RoutingConditionTypeUserAgent, Op: RoutingConditionOpRegex, Value: `claude-cli/2\.`}},
		AccountIDs: []int64{401},
	})
	dec = svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "x", UserAgent: "claude-cli/2.1.0"})
	if !dec.HasMatch() {
		t.Fatalf("expected ua regex match, got %+v", dec)
	}
	dec = svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "x", UserAgent: "claude-cli/1.0.0"})
	if dec.HasMatch() {
		t.Fatalf("expected ua regex no match for v1, got %+v", dec)
	}
}

func TestRoutingEvaluate_MatchModeAllVsAny(t *testing.T) {
	conds := []RoutingCondition{
		{Type: RoutingConditionTypeModel, Op: RoutingConditionOpWildcard, Value: "claude-opus-*"},
		{Type: RoutingConditionTypeClient, Value: RoutingClientClaudeCode},
	}
	// all: 需两者都满足
	svcAll := newTestRoutingService(RoutingStrategy{
		ID: 5, Name: "all", Enabled: true, Priority: 10, Platform: PlatformAnthropic,
		Action: RoutingActionRestrict, MatchMode: RoutingMatchModeAll, Conditions: conds, AccountIDs: []int64{501},
	})
	if svcAll.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus-4", ClientType: RoutingClientOther}).HasMatch() {
		t.Fatal("all-mode should not match when client differs")
	}
	if !svcAll.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus-4", ClientType: RoutingClientClaudeCode}).HasMatch() {
		t.Fatal("all-mode should match when both satisfied")
	}

	// any: 任一满足即可
	svcAny := newTestRoutingService(RoutingStrategy{
		ID: 6, Name: "any", Enabled: true, Priority: 10, Platform: PlatformAnthropic,
		Action: RoutingActionRestrict, MatchMode: RoutingMatchModeAny, Conditions: conds, AccountIDs: []int64{601},
	})
	if !svcAny.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus-4", ClientType: RoutingClientOther}).HasMatch() {
		t.Fatal("any-mode should match when model satisfied")
	}
}

func TestRoutingEvaluate_FirstMatchWins(t *testing.T) {
	svc := newTestRoutingService(
		RoutingStrategy{
			ID: 7, Name: "low-priority-first", Enabled: true, Priority: 5, Platform: PlatformAnthropic,
			Action: RoutingActionRestrict, MatchMode: RoutingMatchModeAll,
			Conditions: []RoutingCondition{{Type: RoutingConditionTypeModel, Op: RoutingConditionOpWildcard, Value: "claude-*"}},
			AccountIDs: []int64{701},
		},
		RoutingStrategy{
			ID: 8, Name: "higher-priority-number", Enabled: true, Priority: 50, Platform: PlatformAnthropic,
			Action: RoutingActionPrefer, MatchMode: RoutingMatchModeAll,
			Conditions: []RoutingCondition{{Type: RoutingConditionTypeModel, Op: RoutingConditionOpWildcard, Value: "claude-opus-*"}},
			AccountIDs: []int64{801},
		},
	)
	// 两条都匹配 opus，但 priority=5 的先评估并胜出
	dec := svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus-4"})
	if dec.MatchedID != 7 || len(dec.RestrictIDs) != 1 || dec.RestrictIDs[0] != 701 {
		t.Fatalf("expected strategy 7 to win, got %+v", dec)
	}
}

func TestRoutingEvaluate_PlatformAndGroupScope(t *testing.T) {
	svc := newTestRoutingService(RoutingStrategy{
		ID: 9, Name: "group-scoped", Enabled: true, Priority: 10, Platform: PlatformAnthropic, GroupID: ptrInt64(42),
		Action: RoutingActionRestrict, MatchMode: RoutingMatchModeAll,
		Conditions: []RoutingCondition{{Type: RoutingConditionTypeModel, Op: RoutingConditionOpWildcard, Value: "claude-*"}},
		AccountIDs: []int64{901},
	})
	// 平台不符
	if svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformGemini, Model: "claude-opus", GroupID: ptrInt64(42)}).HasMatch() {
		t.Fatal("should not match on different platform")
	}
	// 分组不符
	if svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus", GroupID: ptrInt64(7)}).HasMatch() {
		t.Fatal("should not match on different group")
	}
	// 分组缺失
	if svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus"}).HasMatch() {
		t.Fatal("should not match when group missing for group-scoped strategy")
	}
	// 命中
	if !svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus", GroupID: ptrInt64(42)}).HasMatch() {
		t.Fatal("should match when platform+group match")
	}
}

func TestRoutingEvaluate_EmptyConditionsCatchAll(t *testing.T) {
	svc := newTestRoutingService(RoutingStrategy{
		ID: 10, Name: "catch-all", Enabled: true, Priority: 100, Platform: "",
		Action: RoutingActionPrefer, MatchMode: RoutingMatchModeAll, Conditions: nil, AccountIDs: []int64{1001},
	})
	dec := svc.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "anything"})
	if !dec.HasMatch() || dec.PreferIDs[0] != 1001 {
		t.Fatalf("expected catch-all match, got %+v", dec)
	}
}

func TestRoutingEvaluate_DisabledServiceSafe(t *testing.T) {
	var svc *RoutingStrategyService // nil service path is handled by gateway, but Evaluate needs a repo
	_ = svc
	// 空策略集 → 无命中
	empty := newTestRoutingService()
	if empty.Evaluate(context.Background(), RoutingMatchContext{Platform: PlatformAnthropic, Model: "claude-opus"}).HasMatch() {
		t.Fatal("empty strategy set should not match")
	}
}
