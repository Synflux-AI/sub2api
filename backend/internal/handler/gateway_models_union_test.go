//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// groupModelIDs 与 boundGroupsModelsUnion 都依赖 gatewayService 取账号可用模型，
// 而本文件要验证的是「合并/过滤的先后顺序」这类纯逻辑。所以这里只测不需要
// gatewayService 的那部分：自定义清单的过滤顺序，以及并集的去重。
//
// 完整的 handler 端到端行为由 routes 层的测试覆盖。

// 关键性质：**每个分组先各自应用自己的 models_list_config，再合并**。
// 反过来（先合并再过滤）会让 A 组的清单误伤 B 组的模型 —— 那是这个实现最容易
// 写错的地方，也是本用例存在的唯一理由。
func TestPerGroupFilterThenMergeNotMergeThenFilter(t *testing.T) {
	// A 组只允许 claude-*，B 组只允许 gpt-*。
	groupA := &service.Group{
		ID: 10, Platform: service.PlatformAnthropic,
		ModelsListConfig: domain.GroupModelsListConfig{
			Enabled: true,
			Models:  []string{"claude-sonnet-4"},
		},
	}
	groupB := &service.Group{
		ID: 20, Platform: service.PlatformOpenAI,
		ModelsListConfig: domain.GroupModelsListConfig{
			Enabled: true,
			Models:  []string{"gpt-5.6"},
		},
	}
	require.True(t, groupA.CustomModelsListEnabled())
	require.True(t, groupB.CustomModelsListEnabled())

	// 模拟「每组先过滤」：各组的清单在各自的候选集里都应保留。
	aFiltered := filterModelsByCustomList(
		[]string{"claude-sonnet-4", "claude-opus-5"},
		defaultModelIDsForPlatform(groupA.Platform),
		groupA.ModelsListConfig.Models,
	)
	bFiltered := filterModelsByCustomList(
		[]string{"gpt-5.6", "gpt-5.6-mini"},
		defaultModelIDsForPlatform(groupB.Platform),
		groupB.ModelsListConfig.Models,
	)
	require.Equal(t, []string{"claude-sonnet-4"}, aFiltered)
	require.Equal(t, []string{"gpt-5.6"}, bFiltered)

	union := mergeModelIDs(aFiltered, bFiltered)
	require.ElementsMatch(t, []string{"claude-sonnet-4", "gpt-5.6"}, union,
		"先各自过滤再合并，两个分组的自定义清单都应完整出现在并集里")

	// 对照：如果反过来先合并候选集、再用其中**一个**分组的清单过滤，
	// 另一个分组的模型就会被吃掉 —— 这正是要避免的错误顺序。
	wrong := filterModelsByCustomList(
		mergeModelIDs([]string{"claude-sonnet-4", "claude-opus-5"}, []string{"gpt-5.6", "gpt-5.6-mini"}),
		nil,
		groupA.ModelsListConfig.Models,
	)
	require.NotContains(t, wrong, "gpt-5.6",
		"这条断言是反面教材：先合并再过滤会丢掉 B 组的模型，实现绝不能这么写")
}

// 并集必须去重：两个分组都暴露同一个模型时只出现一次，且保持首次出现的顺序。
func TestModelsUnionDeduplicatesAcrossGroups(t *testing.T) {
	union := mergeModelIDs(
		[]string{"claude-sonnet-4", "shared-model"},
		[]string{"shared-model", "gpt-5.6"},
	)
	require.Equal(t, []string{"claude-sonnet-4", "shared-model", "gpt-5.6"}, union)
}

// 单分组 / 未分组 Key 必须走原有逻辑：boundGroupsModelsUnion 直接放弃。
func TestBoundGroupsModelsUnionSkipsSingleGroupKeys(t *testing.T) {
	h := &GatewayHandler{}
	single := &service.Group{ID: 10, Platform: service.PlatformAnthropic}

	for name, apiKey := range map[string]*service.APIKey{
		"nil key": nil,
		"未分组":     {},
		"单分组": {
			GroupID: &single.ID, Group: single,
			BoundGroups: []*service.Group{single},
		},
	} {
		t.Run(name, func(t *testing.T) {
			models, platform, ok := h.boundGroupsModelsUnion(nil, apiKey)
			require.False(t, ok, "非多分组 Key 必须交回原有单分组逻辑")
			require.Nil(t, models)
			require.Empty(t, platform)
		})
	}
}
