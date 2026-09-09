//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// #189：错误处理规则从 Anthropic 专属扩展成按平台可配，并加一条「上游耗时上限」条件。
//
// 两个字段都用裸值而不是指针：
//   - Platforms：validate 要求非空，「全平台」= 全部勾上。于是 len==0 唯一地表示
//     「存量配置没有这个字段」，normalize 补 ["anthropic"]，升级后行为零变化。
//   - MaxUpstreamLatencyMs：0 是天然哨兵（「阈值 0ms」不是合法配置值，会永不命中）。

func TestNormalizeErrorHandlingRulesBackfillsLegacyPlatformsAsAnthropic(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "legacy", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
	}
	normalizeErrorHandlingRules(rules)
	require.Equal(t, []string{PlatformAnthropic}, rules[0].Platforms,
		"存量规则没有 platforms，必须收窄成 anthropic，否则升级瞬间对 OpenAI 生效")
}

func TestNormalizeErrorHandlingRulesLowercasesTrimsAndDedupesPlatforms(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "a", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry,
			Platforms: []string{" OpenAI ", "openai", "ANTHROPIC", "  "}},
	}
	normalizeErrorHandlingRules(rules)
	require.Equal(t, []string{PlatformOpenAI, PlatformAnthropic}, rules[0].Platforms)
}

func TestErrorHandlingRuleMatchesPlatform(t *testing.T) {
	rule := ErrorHandlingRule{Platforms: []string{PlatformOpenAI}}
	require.True(t, rule.MatchesPlatform(PlatformOpenAI))
	require.True(t, rule.MatchesPlatform(" OpenAI "))
	require.False(t, rule.MatchesPlatform(PlatformAnthropic))

	both := ErrorHandlingRule{Platforms: []string{PlatformAnthropic, PlatformOpenAI}}
	require.True(t, both.MatchesPlatform(PlatformAnthropic))
	require.True(t, both.MatchesPlatform(PlatformOpenAI))

	// 空平台列表只可能来自「还没 normalize」的中间态；此时不做过滤，交给 normalize 兜底。
	legacy := ErrorHandlingRule{}
	require.True(t, legacy.MatchesPlatform(PlatformAnthropic))
	require.True(t, legacy.MatchesPlatform(PlatformOpenAI))
}

func TestErrorHandlingRuleMatchesUpstreamLatency(t *testing.T) {
	tests := []struct {
		name      string
		threshold int
		latencyMs int
		want      bool
	}{
		{"阈值 0 = 不限制，任何耗时都命中", 0, 41_000, true},
		{"阈值 0 = 不限制，耗时未知也命中", 0, 0, true},
		{"耗时低于阈值，命中", 5_000, 504, true},
		{"耗时等于阈值，不命中", 5_000, 5_000, false},
		{"耗时高于阈值，不命中", 5_000, 41_000, false},
		// fail-closed：这个字段的动机就是防重复扣费，没量到耗时的路径上应该保守。
		// 若按 0 < 5000 放行，一条本意为「只在快速失败时重试」的规则会在所有没插桩的
		// 路径上无条件重试。
		{"耗时未知（0），不满足任何已配置阈值", 5_000, 0, false},
		{"耗时未知（负数），不满足任何已配置阈值", 5_000, -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := ErrorHandlingRule{MaxUpstreamLatencyMs: tt.threshold}
			require.Equal(t, tt.want, rule.MatchesUpstreamLatency(tt.latencyMs))
		})
	}
}

func TestMatchErrorHandlingRuleFilteredByPlatform(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "openai-only", StatusCodes: []int{502}, Action: ErrorHandlingActionFailover,
			Platforms: []string{PlatformOpenAI}},
		{ID: "anthropic-only", StatusCodes: []int{502}, Action: ErrorHandlingActionRetry,
			Platforms: []string{PlatformAnthropic}},
	}

	got := matchErrorHandlingRuleFiltered(rules, errorHandlingRuleMatchFilter{Platform: PlatformOpenAI}, 502, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "openai-only", got.ID)

	got = matchErrorHandlingRuleFiltered(rules, errorHandlingRuleMatchFilter{Platform: PlatformAnthropic}, 502, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "anthropic-only", got.ID)

	got = matchErrorHandlingRuleFiltered(rules, errorHandlingRuleMatchFilter{Platform: PlatformGemini}, 502, []byte(`{}`))
	require.Nil(t, got, "没有任何规则勾选 gemini，必须不命中")
}

func TestMatchErrorHandlingRuleFilteredByUpstreamLatency(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "fast-fail-only", StatusCodes: []int{502}, Action: ErrorHandlingActionFailover,
			Platforms: []string{PlatformOpenAI}, MaxUpstreamLatencyMs: 5_000},
	}
	filter := func(latency int) errorHandlingRuleMatchFilter {
		return errorHandlingRuleMatchFilter{Platform: PlatformOpenAI, UpstreamLatencyMs: latency}
	}

	require.NotNil(t, matchErrorHandlingRuleFiltered(rules, filter(504), 502, []byte(`{}`)),
		"504ms 失败：重试免费，应该命中")
	require.Nil(t, matchErrorHandlingRuleFiltered(rules, filter(41_000), 502, []byte(`{}`)),
		"41s 失败：图很可能已生成已计费，重试等于重复扣费，不应命中")
	require.Nil(t, matchErrorHandlingRuleFiltered(rules, filter(0), 502, []byte(`{}`)),
		"耗时未知：保守，不命中")
}

func TestMatchErrorHandlingRuleFilteredSkipsPlatformFilterWhenUnset(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "anthropic-only", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry,
			Platforms: []string{PlatformAnthropic}},
	}
	got := matchErrorHandlingRuleFiltered(rules, errorHandlingRuleMatchFilter{}, 500, []byte(`{}`))
	require.NotNil(t, got, "Platform 为空表示调用方没有平台上下文，不做平台过滤")
}

func TestGetErrorHandlingRuleSettingsBackfillsPlatformsForLegacyJSON(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyErrorHandlingRules: `{"enabled":true,"default_retry_count":1,"rules":[` +
			`{"id":"legacy","status_codes":[429],"action":"retry"},` +
			`{"id":"explicit","status_codes":[502],"action":"failover","platforms":["openai"]}]}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetErrorHandlingRuleSettings(context.Background())
	require.NoError(t, err)
	require.Len(t, settings.Rules, 2)
	require.Equal(t, []string{PlatformAnthropic}, settings.Rules[0].Platforms)
	require.Equal(t, []string{PlatformOpenAI}, settings.Rules[1].Platforms)
	require.Zero(t, settings.Rules[0].MaxUpstreamLatencyMs, "存量规则没有耗时门限，必须是 0 = 不限制")
}

func TestValidateErrorHandlingRulePlatformsAndLatency(t *testing.T) {
	tests := []struct {
		name    string
		rule    ErrorHandlingRule
		wantErr bool
	}{
		{"合法：anthropic", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			Platforms: []string{PlatformAnthropic}}, false},
		{"合法：openai", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			Platforms: []string{PlatformOpenAI}}, false},
		{"合法：两个都勾（= 全平台）", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			Platforms: []string{PlatformAnthropic, PlatformOpenAI}}, false},
		// #228 task-13 打开写入白名单后，8 个具体平台都能勾：改用 isConcreteRequestPlatform
		// 而不是固定的两平台 map，Gemini 不再是"引擎未接线的平台"。
		{"合法：gemini（task-13 打开白名单后）", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			Platforms: []string{PlatformGemini}}, false},
		// composite 是分组层虚拟平台，account.Platform 永远是具体平台，收进来只会多一个
		// 永不命中的框，必须继续拒绝。
		{"非法：composite（虚拟平台）", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			Platforms: []string{PlatformComposite}}, true},
		{"非法：不存在的平台名", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			Platforms: []string{"banana"}}, true},
		{"合法：耗时门限 0", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			MaxUpstreamLatencyMs: 0}, false},
		{"非法：耗时门限为负", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			MaxUpstreamLatencyMs: -1}, true},
		{"非法：耗时门限超上限", ErrorHandlingRule{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry,
			MaxUpstreamLatencyMs: errorHandlingRuleMaxUpstreamLatencyMs + 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{tt.rule}}
			normalizeErrorHandlingRules(settings.Rules)
			err := validateErrorHandlingRuleSettings(settings)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrErrorHandlingRuleSettingsInvalid)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSetErrorHandlingRuleSettingsRoundTripsPlatformsAndLatency(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	require.NoError(t, svc.SetErrorHandlingRuleSettings(ctx, &ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []ErrorHandlingRule{{
			ID: "images-lost-ping", StatusCodes: []int{502}, Keywords: []string{"connection lost"},
			Action: ErrorHandlingActionFailover, Platforms: []string{PlatformOpenAI},
			MaxUpstreamLatencyMs: 5_000,
		}},
	}))

	got, err := svc.GetErrorHandlingRuleSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{PlatformOpenAI}, got.Rules[0].Platforms)
	require.Equal(t, 5_000, got.Rules[0].MaxUpstreamLatencyMs)
}

func TestHasEnabledErrorHandlingRuleForPlatform(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "openai", StatusCodes: []int{502}, Action: ErrorHandlingActionFailover, Platforms: []string{PlatformOpenAI}},
	}
	require.True(t, HasEnabledErrorHandlingRuleForPlatform(rules, PlatformOpenAI))
	require.False(t, HasEnabledErrorHandlingRuleForPlatform(rules, PlatformAnthropic),
		"没有一条规则勾了 anthropic 时，热路径不该为了 decide 一遍去读错误响应体")
}

// #228 task-13：写入白名单从「只认 anthropic/openai」的固定 map 换成
// isConcreteRequestPlatform（本仓库「具体请求平台」的单一权威清单）。

// 8 平台 × 勾选组合。写入白名单必须与 isConcreteRequestPlatform 完全一致：
// 两份清单会漂移，而漂移的后果是管理台出现「勾了却存不下」或「存下了却不生效」。
func TestErrorHandlingRuleValidateAcceptsAllConcretePlatforms(t *testing.T) {
	for _, platform := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity,
		PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek,
	} {
		t.Run(platform, func(t *testing.T) {
			settings := execTestSettings(ErrorHandlingActionFailover, 1, "", []string{platform})
			if err := validateErrorHandlingRuleSettings(&settings); err != nil {
				t.Fatalf("platform %q must be accepted, got %v", platform, err)
			}
		})
	}
}

// composite 不该被接受：account.Platform 永远是具体平台，收进来只会多一个永不命中的框。
func TestErrorHandlingRuleValidateRejectsCompositePlatform(t *testing.T) {
	settings := execTestSettings(ErrorHandlingActionFailover, 1, "", []string{PlatformComposite})
	if err := validateErrorHandlingRuleSettings(&settings); err == nil {
		t.Fatal("composite must be rejected: it is a group-level virtual platform")
	}
}

// 白名单与 isConcreteRequestPlatform 不得漂移。
func TestErrorHandlingRulePlatformWhitelistMatchesConcretePlatforms(t *testing.T) {
	for _, platform := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity,
		PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek,
		PlatformComposite, "nonsense",
	} {
		settings := execTestSettings(ErrorHandlingActionFailover, 1, "", []string{platform})
		accepted := validateErrorHandlingRuleSettings(&settings) == nil
		if accepted != isConcreteRequestPlatform(platform) {
			t.Errorf("platform %q: validate accepted=%v but isConcreteRequestPlatform=%v",
				platform, accepted, isConcreteRequestPlatform(platform))
		}
	}
}
