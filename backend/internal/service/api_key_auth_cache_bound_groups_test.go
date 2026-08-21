package service

import (
	"context"
	"testing"
)

// TestAPIKeyService_RejectsV22AuthSnapshotWithoutBoundGroups 是本次上线期最重要的一条护栏。
//
// 存量 v22 快照没有 groups 字段，反序列化后 len(Groups)==0。而在 v23 的语义里
// 「绑定集合为空」表示未分组 Key。若不按版本号判废，多分组 Key 会在 L2 TTL（默认 300s）内
// 被还原成只有默认组的对象：选组失效、全部请求按默认组计费，**无报错、无日志**。
func TestAPIKeyService_RejectsV22AuthSnapshotWithoutBoundGroups(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-bound-groups", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  22,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeStandard,
				RateMultiplier:   1,
			},
			// 刻意不设 Groups —— 这正是存量 v22 快照的形状。
		},
	})

	if err != nil {
		t.Fatalf("过期快照应被静默忽略而非报错，得到 %v", err)
	}
	if ok {
		t.Fatalf("v22 快照缺 groups 字段，必须被判废回源，否则多分组 Key 会静默退化成单分组")
	}
	if apiKey != nil {
		t.Fatalf("判废的快照不应产出 APIKey，得到 %#v", apiKey)
	}
}

// TestSnapshotRoundTripPreservesBoundGroups 验证 APIKey ↔ 快照 的绑定集合往返：
// 顺序、字段、Hydrated 都要保住。选组依赖这个集合，还原不出来就等于没有多分组。
func TestSnapshotRoundTripPreservesBoundGroups(t *testing.T) {
	svc := &APIKeyService{}
	anthropic := &Group{ID: 10, Name: "claude-ccmax", Platform: PlatformAnthropic, Status: StatusActive, RateMultiplier: 1.0, Hydrated: true}
	openai := &Group{ID: 20, Name: "codex", Platform: PlatformOpenAI, Status: StatusActive, RateMultiplier: 1.2, Hydrated: true}

	src := &APIKey{
		ID:          1,
		UserID:      2,
		Key:         "sk-x",
		Status:      StatusActive,
		GroupID:     &anthropic.ID,
		Group:       anthropic,
		BoundGroups: []*Group{anthropic, openai},
		User:        &User{ID: 2, Status: StatusActive, Role: RoleUser},
	}

	snap := svc.snapshotFromAPIKey(context.Background(), src)
	if got := len(snap.Groups); got != 2 {
		t.Fatalf("快照应带 2 个绑定分组，得到 %d", got)
	}
	if snap.Groups[0].ID != anthropic.ID || snap.Groups[1].ID != openai.ID {
		t.Fatalf("绑定集合顺序必须与 BoundGroups 一致（选组依赖它稳定），得到 %d,%d",
			snap.Groups[0].ID, snap.Groups[1].ID)
	}

	restored := svc.snapshotToAPIKey("sk-x", snap)
	if got := len(restored.BoundGroups); got != 2 {
		t.Fatalf("还原应得到 2 个绑定分组，得到 %d", got)
	}
	for i, g := range restored.BoundGroups {
		if !g.Hydrated {
			t.Fatalf("还原的绑定分组[%d] 必须 Hydrated=true，否则 setGroupContext 会静默跳过", i)
		}
		if !IsGroupContextValid(g) {
			t.Fatalf("还原的绑定分组[%d] 必须能通过 IsGroupContextValid", i)
		}
	}
	if restored.BoundGroups[1].RateMultiplier != 1.2 {
		t.Fatalf("非默认绑定分组的倍率必须原样还原（这是按命中分组独立计费的前提），得到 %v",
			restored.BoundGroups[1].RateMultiplier)
	}
	// 默认组仍是默认组，且指针语义不变。
	if restored.GroupID == nil || *restored.GroupID != anthropic.ID {
		t.Fatalf("默认组指针必须保持不变")
	}
	if restored.Group == nil || restored.Group.ID != anthropic.ID {
		t.Fatalf("默认组对象必须保持不变")
	}
}

// 未分组 Key 的快照不得凭空长出绑定集合（C5）。
func TestSnapshotRoundTripUngroupedKeyStaysUngrouped(t *testing.T) {
	svc := &APIKeyService{}
	src := &APIKey{ID: 1, UserID: 2, Key: "sk-y", Status: StatusActive, User: &User{ID: 2}}

	snap := svc.snapshotFromAPIKey(context.Background(), src)
	if len(snap.Groups) != 0 {
		t.Fatalf("未分组 Key 的快照不应有绑定分组，得到 %d", len(snap.Groups))
	}
	restored := svc.snapshotToAPIKey("sk-y", snap)
	if len(restored.BoundGroups) != 0 {
		t.Fatalf("未分组 Key 还原后不应有绑定分组，得到 %d", len(restored.BoundGroups))
	}
	if restored.GroupID != nil || restored.Group != nil {
		t.Fatalf("未分组 Key 的默认组必须仍为 nil")
	}
}
