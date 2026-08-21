package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// issue #171 的核心验收之一：一把 Key 绑两个倍率不同的分组时，
// GET /v1/sub2api/billing 必须把**每个分组各自的倍率**都报出来，
// 而不是只报默认组那一个。
func TestKeyBillingInfoReportsPerGroupMultipliers(t *testing.T) {
	anthropicID, openaiID := int64(10), int64(20)
	anthropic := &service.Group{
		ID: anthropicID, Name: "claude-ccmax",
		Platform: service.PlatformAnthropic, RateMultiplier: 1.0,
	}
	openai := &service.Group{
		ID: openaiID, Name: "codex",
		Platform: service.PlatformOpenAI, RateMultiplier: 1.2,
	}
	apiKey := &service.APIKey{
		UserID:      11,
		GroupID:     &anthropicID,
		Group:       anthropic,
		BoundGroups: []*service.Group{anthropic, openai},
	}

	c, w := newKeyBillingContext(apiKey)
	newKeyBillingHandler(nil).KeyBillingInfo(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got keyBillingInfoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))

	// 顶层字段保持 v1 语义 = **默认组**，旧客户端零改动继续工作。
	require.Equal(t, 2, got.SchemaVersion)
	require.Equal(t, 1.0, got.GroupRateMultiplier, "顶层倍率必须仍是默认组的")
	require.Equal(t, 1.0, got.EffectiveRateMultiplier)

	// groups[] 逐个分组给出自己的倍率。
	require.Len(t, got.Groups, 2)
	byPlatform := map[string]keyBillingGroupInfo{}
	for _, g := range got.Groups {
		byPlatform[g.Platform] = g
	}

	anth, ok := byPlatform[service.PlatformAnthropic]
	require.True(t, ok, "缺 anthropic 分组的倍率条目")
	require.True(t, anth.IsDefault, "默认组条目必须标记 is_default")
	require.Equal(t, 1.0, anth.GroupRateMultiplier)
	require.Equal(t, 1.0, anth.EffectiveRateMultiplier)

	oai, ok := byPlatform[service.PlatformOpenAI]
	require.True(t, ok, "缺 openai 分组的倍率条目")
	require.False(t, oai.IsDefault)
	require.Equal(t, 1.2, oai.GroupRateMultiplier,
		"非默认分组必须报它自己的倍率——这正是 issue #171 要解决的问题")
	require.Equal(t, 1.2, oai.EffectiveRateMultiplier)
}

// 峰值倍率也要按分组各自算：只有开了峰值的那个分组才带峰值字段。
func TestKeyBillingInfoPerGroupPeakRate(t *testing.T) {
	anthropicID, openaiID := int64(10), int64(20)
	anthropic := &service.Group{
		ID: anthropicID, Platform: service.PlatformAnthropic, RateMultiplier: 1.0,
	}
	openai := &service.Group{
		ID: openaiID, Platform: service.PlatformOpenAI, RateMultiplier: 1.0,
		// 峰值倍率只对**订阅类型**分组生效（service.Group.PeakMultiplierAt 的前置条件，
		// 也是 ValidatePeakRateConfig 的既有规则）。夹具必须显式设成订阅型，
		// 否则测到的是「非订阅组不应用峰值」而不是「峰值按分组各自算」。
		SubscriptionType: service.SubscriptionTypeSubscription,
		PeakRateEnabled:  true, PeakStart: "00:00", PeakEnd: "23:59", PeakRateMultiplier: 2.0,
	}
	apiKey := &service.APIKey{
		UserID: 11, GroupID: &anthropicID, Group: anthropic,
		BoundGroups: []*service.Group{anthropic, openai},
	}

	c, w := newKeyBillingContext(apiKey)
	newKeyBillingHandler(nil).KeyBillingInfo(c)
	require.Equal(t, http.StatusOK, w.Code)

	var got keyBillingInfoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.False(t, got.PeakRateEnabled, "默认组没开峰值，顶层字段必须仍是 false")

	byPlatform := map[string]keyBillingGroupInfo{}
	for _, g := range got.Groups {
		byPlatform[g.Platform] = g
	}
	require.False(t, byPlatform[service.PlatformAnthropic].PeakRateEnabled)
	require.Nil(t, byPlatform[service.PlatformAnthropic].AppliedPeakMultiplier)

	oai := byPlatform[service.PlatformOpenAI]
	require.True(t, oai.PeakRateEnabled)
	require.NotNil(t, oai.PeakRateMultiplier)
	require.Equal(t, 2.0, *oai.PeakRateMultiplier, "配置值原样透传，与当前时间无关")
	require.NotNil(t, oai.AppliedPeakMultiplier)

	// 刻意**不**断言 applied 的具体数值：它取决于跑测试的挂钟时间是否落在峰值窗口内。
	// 硬编码 2.0 会让这条用例在窗口边界那一分钟 flaky（本仓库有时间/负载敏感 flake 的
	// 历史）。这里断言的是真正要验证的性质：峰值因子是**按该分组自己的配置**算出来的，
	// 并且乘进了这个分组自己的 effective 倍率。
	require.Equal(t, oai.ResolvedRateMultiplier*(*oai.AppliedPeakMultiplier), oai.EffectiveRateMultiplier,
		"分组的 effective 倍率必须等于它自己的 resolved × 它自己的 applied peak")
	require.Contains(t, []float64{1.0, 2.0}, *oai.AppliedPeakMultiplier,
		"applied 只可能是 1.0（窗口外）或配置的 2.0（窗口内）")
}

// 这个端点是客户端可见的：既有测试已把「不得泄露分组名」与「不得泄露 key」
// 并列断言。新增的 groups[] 同样不能开这个口子。
func TestKeyBillingInfoDoesNotLeakGroupIdentity(t *testing.T) {
	anthropicID, openaiID := int64(10), int64(20)
	anthropic := &service.Group{
		ID: anthropicID, Name: "internal-vip-customer-alpha",
		Platform: service.PlatformAnthropic, RateMultiplier: 1.0,
	}
	openai := &service.Group{
		ID: openaiID, Name: "internal-reseller-beta",
		Platform: service.PlatformOpenAI, RateMultiplier: 1.2,
	}
	apiKey := &service.APIKey{
		UserID: 11, Key: "sk-super-secret", GroupID: &anthropicID, Group: anthropic,
		BoundGroups: []*service.Group{anthropic, openai},
	}

	c, w := newKeyBillingContext(apiKey)
	newKeyBillingHandler(nil).KeyBillingInfo(c)
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	require.NotContains(t, body, anthropic.Name, "不得泄露分组名（分组名常编码内部/商务信息）")
	require.NotContains(t, body, openai.Name)
	require.NotContains(t, body, apiKey.Key)

	// group_id / group_name 这两个字段名本身也不该出现 —— platform 已是完备区分键。
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	groups, _ := raw["groups"].([]any)
	require.Len(t, groups, 2)
	for _, g := range groups {
		entry, _ := g.(map[string]any)
		require.NotContains(t, entry, "group_id")
		require.NotContains(t, entry, "group_name")
		require.Contains(t, entry, "platform")
	}
}
