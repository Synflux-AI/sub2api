package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// issue #171：响应必须带上完整的绑定集合，前端编辑表单才能把它原样塞回 group_ids。
func TestAPIKeyFromServiceExposesBoundGroups(t *testing.T) {
	anthropic := &service.Group{ID: 10, Name: "claude-ccmax", Platform: service.PlatformAnthropic, RateMultiplier: 1.0}
	openai := &service.Group{ID: 20, Name: "codex", Platform: service.PlatformOpenAI, RateMultiplier: 1.2}

	got := APIKeyFromService(&service.APIKey{
		ID: 1, UserID: 2, GroupID: &anthropic.ID, Group: anthropic,
		BoundGroups: []*service.Group{anthropic, openai},
	})
	require.NotNil(t, got)

	require.Equal(t, []int64{10, 20}, got.GroupIDs, "顺序必须与读路径的 (platform, id) 排序一致")
	require.Len(t, got.Groups, 2)
	require.Equal(t, int64(10), got.Groups[0].ID)
	require.Equal(t, int64(20), got.Groups[1].ID)

	// 列表页要直接渲染「每个平台走哪个分组、倍率多少」，所以 platform 与倍率必须带上。
	require.Equal(t, service.PlatformAnthropic, got.Groups[0].Platform)
	require.Equal(t, 1.0, got.Groups[0].RateMultiplier)
	require.Equal(t, service.PlatformOpenAI, got.Groups[1].Platform)
	require.Equal(t, 1.2, got.Groups[1].RateMultiplier)

	// 旧字段语义不变：group_id / group 仍是**默认组**，旧前端零改动可用。
	require.NotNil(t, got.GroupID)
	require.Equal(t, int64(10), *got.GroupID)
	require.NotNil(t, got.Group)
	require.Equal(t, int64(10), got.Group.ID)
}

// 未分组 / 单分组 Key 也要输出 group_ids（可能是空数组），
// 这样前端可以无条件读它，不必判断字段是否存在。
func TestAPIKeyFromServiceGroupIDsAlwaysPresent(t *testing.T) {
	t.Run("未分组", func(t *testing.T) {
		got := APIKeyFromService(&service.APIKey{ID: 1})
		require.NotNil(t, got)
		require.NotNil(t, got.GroupIDs, "必须是空数组而不是 nil —— nil 会序列化成 null")
		require.Empty(t, got.GroupIDs)
		require.Empty(t, got.Groups)

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &fields))
		require.Contains(t, fields, "group_ids")
		require.JSONEq(t, `[]`, string(fields["group_ids"]))
		// groups 带 omitempty，未加载时不占响应体积。
		require.NotContains(t, fields, "groups")
	})

	t.Run("单分组", func(t *testing.T) {
		only := &service.Group{ID: 7, Platform: service.PlatformAnthropic}
		got := APIKeyFromService(&service.APIKey{
			ID: 1, GroupID: &only.ID, Group: only,
			BoundGroups: []*service.Group{only},
		})
		require.Equal(t, []int64{7}, got.GroupIDs)
		require.Len(t, got.Groups, 1)
	})
}

// BoundGroups 里的 nil 元素不能把响应搞崩，也不能产生一个空对象。
func TestAPIKeyFromServiceSkipsNilBoundGroups(t *testing.T) {
	real := &service.Group{ID: 9, Platform: service.PlatformGemini}
	got := APIKeyFromService(&service.APIKey{
		ID: 1, BoundGroups: []*service.Group{nil, real, nil},
	})
	require.Equal(t, []int64{9}, got.GroupIDs)
	require.Len(t, got.Groups, 1)
	require.Equal(t, int64(9), got.Groups[0].ID)
}
