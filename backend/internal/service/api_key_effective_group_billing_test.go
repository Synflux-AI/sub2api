//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// issue #171 的核心验收：倍率必须按**实际命中的分组**算，而不是默认组。
//
// 整条计费链路（rate_multiplier、峰值、媒体、user:group 覆盖、usage_log.group_id）
// 都是从 apiKey.Group / apiKey.GroupID 上读的。选组之后我们用
// CloneAPIKeyWithGroup 把这两个字段换成命中分组，下游 700+ 处裸读因此自动跟随。
//
// 这个文件钉住的就是那个「换一次、全链路跟随」的性质 —— 它是本 issue 全部计费正确性
// 的支点。如果 clone 漏了某个字段，或者某个倍率函数绕过 apiKey.Group 去读别处，
// 这里的断言就会挂。

func multiGroupBillingKey() (*APIKey, *Group, *Group) {
	anthropic := &Group{
		ID: 10, Name: "claude-ccmax", Platform: PlatformAnthropic,
		Status: StatusActive, SubscriptionType: SubscriptionTypeStandard,
		RateMultiplier: 1.0,
		// 默认组：不开峰值、不开媒体独立倍率。
		Hydrated: true,
	}
	openai := &Group{
		ID: 20, Name: "codex", Platform: PlatformOpenAI,
		Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription,
		RateMultiplier: 1.2,
		// 命中组：开峰值（全天窗口，确保命中）+ 图片/视频独立倍率。
		PeakRateEnabled: true, PeakStart: "00:00", PeakEnd: "23:59", PeakRateMultiplier: 3.0,
		ImageRateIndependent: true, ImageRateMultiplier: 7.0,
		VideoRateIndependent: true, VideoRateMultiplier: 9.0,
		Hydrated: true,
	}
	key := &APIKey{
		ID: 1, UserID: 7, Key: "sk-mg", Status: StatusActive,
		GroupID: &anthropic.ID, Group: anthropic,
		BoundGroups: []*Group{anthropic, openai},
		User:        &User{ID: 7, Status: StatusActive},
	}
	return key, anthropic, openai
}

func TestCloneAPIKeyWithGroupSwitchesEntireBillingBasis(t *testing.T) {
	key, anthropic, openai := multiGroupBillingKey()

	effective := CloneAPIKeyWithGroup(key, openai)
	require.NotNil(t, effective)
	require.NotSame(t, key, effective, "必须是拷贝：原地改会污染认证缓存里的共享对象")

	// 原对象一个字段都不能变 —— 同一把 Key 的其它并发请求还在用它。
	require.Equal(t, anthropic.ID, *key.GroupID)
	require.Same(t, anthropic, key.Group)

	// 拷贝上的「生效分组」两个字段都换了。usage_log.group_id 取的正是 GroupID。
	require.Equal(t, openai.ID, *effective.GroupID,
		"usage_log.group_id 直接取 apiKey.GroupID，必须是命中分组")
	require.Same(t, openai, effective.Group)

	// 倍率取的是命中分组自己的值。
	require.Equal(t, 1.2, effective.Group.RateMultiplier)
}

// 峰值倍率必须用命中分组的配置。默认组没开峰值，命中组开了 3.0 ——
// 若错读默认组，text 倍率就会少乘这 3 倍。
func TestPeakMultiplierFollowsEffectiveGroup(t *testing.T) {
	key, _, openai := multiGroupBillingKey()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	// 改造前的行为（默认组）：没开峰值 -> 1.0。
	require.Equal(t, 1.0, key.Group.PeakMultiplierAt(now),
		"夹具前提：默认组不应用峰值")

	effective := CloneAPIKeyWithGroup(key, openai)
	require.Equal(t, 3.0, effective.Group.PeakMultiplierAt(now),
		"命中组开了峰值，必须用它的倍率")

	base := effective.Group.RateMultiplier
	text, _ := computePeakAwareMultipliers(effective, base, now)
	require.Equal(t, base*3.0, text,
		"token 倍率 = 命中组的 rate_multiplier × 命中组的峰值因子")

	// 对照：拿默认组算出来的 text 倍率完全不同 —— 这就是「读错分组」的后果。
	defaultText, _ := computePeakAwareMultipliers(key, key.Group.RateMultiplier, now)
	require.NotEqual(t, text, defaultText)
}

// 媒体（图片/视频）独立倍率同样必须按命中分组取。
func TestMediaMultipliersFollowEffectiveGroup(t *testing.T) {
	key, _, openai := multiGroupBillingKey()
	effective := CloneAPIKeyWithGroup(key, openai)

	// 默认组没开独立倍率 -> 回退到传入的 base。
	require.Equal(t, 2.5, resolveImageRateMultiplier(key, 2.5),
		"夹具前提：默认组未开图片独立倍率，应回退 base")
	require.Equal(t, 2.5, resolveVideoRateMultiplier(key, 2.5))

	// 命中组开了独立倍率 -> 用它自己的值，忽略 base。
	require.Equal(t, 7.0, resolveImageRateMultiplier(effective, 2.5),
		"命中组开了图片独立倍率，必须用 7.0 而不是 base")
	require.Equal(t, 9.0, resolveVideoRateMultiplier(effective, 2.5))
}

// 订阅类型也跟着命中分组走 —— 认证阶段的订阅分支就是按它判定的。
func TestSubscriptionTypeFollowsEffectiveGroup(t *testing.T) {
	key, _, openai := multiGroupBillingKey()
	require.False(t, key.Group.IsSubscriptionType(), "夹具前提：默认组是标准组")

	effective := CloneAPIKeyWithGroup(key, openai)
	require.True(t, effective.Group.IsSubscriptionType(),
		"命中组是订阅组，认证必须走订阅分支而不是余额分支")
}

// clone 是浅拷贝：User 指针共享。这一点必须显式钉住，
// 因为 checkRPM 会从 User.UserGroupRPMOverrides 里按命中分组的 ID 取值 ——
// 共享是**正确的**，map 本身按分组索引，换组不需要动它。
func TestCloneSharesUserSoPerGroupRPMOverridesStillResolve(t *testing.T) {
	key, anthropic, openai := multiGroupBillingKey()
	key.User.UserGroupRPMOverrides = map[int64]int{anthropic.ID: 1, openai.ID: 5}

	effective := CloneAPIKeyWithGroup(key, openai)
	require.Same(t, key.User, effective.User, "User 是浅拷贝共享的")

	// checkRPM 会用 effective.Group.ID 去查这个 map。
	got, ok := effective.User.UserGroupRPMOverrides[effective.Group.ID]
	require.True(t, ok)
	require.Equal(t, 5, got, "必须取到命中分组的 override，而不是默认组的 1")
}

// 未分组 / nil 入参不得 panic，也不得凭空造出分组。
func TestCloneAPIKeyWithGroupEdgeCases(t *testing.T) {
	require.Nil(t, CloneAPIKeyWithGroup(nil, &Group{ID: 1}))

	key, _, _ := multiGroupBillingKey()
	require.Same(t, key, CloneAPIKeyWithGroup(key, nil),
		"目标分组为 nil 时原样返回入参，不做拷贝也不清空分组")
}
