//go:build unit

package service

import (
	"context"
	"math"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

func modelRateTestGroup() *Group {
	return &Group{
		ID:             7,
		RateMultiplier: 0.8,
		ModelRateMultipliers: []GroupModelRateMultiplier{
			{Models: []string{"example-family-*"}, RateMultiplier: 1.0},
			{Models: []string{"example-family-small", "Example-Other"}, RateMultiplier: 0.6},
		},
	}
}

func TestGroupMatchModelRateMultiplier_ExactBeatsEarlierWildcard(t *testing.T) {
	g := modelRateTestGroup()

	pattern, rate, ok := g.MatchModelRateMultiplier("example-family-small")
	require.True(t, ok)
	require.Equal(t, "example-family-small", pattern)
	require.Equal(t, 0.6, rate)

	pattern, rate, ok = g.MatchModelRateMultiplier("example-family-large")
	require.True(t, ok)
	require.Equal(t, "example-family-*", pattern)
	require.Equal(t, 1.0, rate)

	// 大小写不敏感，返回的是规则里原样保存的模式串。
	pattern, _, ok = g.MatchModelRateMultiplier("example-other")
	require.True(t, ok)
	require.Equal(t, "Example-Other", pattern)

	_, _, ok = g.MatchModelRateMultiplier("unrelated-model")
	require.False(t, ok)
	_, _, ok = g.MatchModelRateMultiplier("")
	require.False(t, ok)

	var nilGroup *Group
	_, _, ok = nilGroup.MatchModelRateMultiplier("example-family-small")
	require.False(t, ok)
}

func TestGroupMatchModelRateMultiplier_ClaudeDotNormalization(t *testing.T) {
	g := &Group{ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"claude-example-4.5"}, RateMultiplier: 0.5}}}
	pattern, rate, ok := g.MatchModelRateMultiplier("claude-example-4-5")
	require.True(t, ok)
	require.Equal(t, "claude-example-4.5", pattern)
	require.Equal(t, 0.5, rate)
}

func TestResolveBillingBaseRate_Precedence(t *testing.T) {
	g := modelRateTestGroup()
	userRate := 0.5

	cases := []struct {
		name       string
		userRate   *float64
		userModels map[string]float64
		model      string
		wantRate   float64
		wantSource BillingRateSource
	}{
		{"group default", nil, nil, "unrelated-model", 0.8, BillingRateSourceGroup},
		{"user rate", &userRate, nil, "unrelated-model", 0.5, BillingRateSourceUser},
		{"group model beats user rate", &userRate, nil, "example-family-small", 0.6, BillingRateSourceGroupModel},
		{"group model without user", nil, nil, "example-family-large", 1.0, BillingRateSourceGroupModel},
		{"user model override", &userRate, map[string]float64{"example-family-small": 0.45}, "example-family-small", 0.45, BillingRateSourceUserModel},
		{"user model override without user rate", nil, map[string]float64{"example-family-*": 0.9}, "example-family-large", 0.9, BillingRateSourceUserModel},
		// 覆盖按命中的分组规则模式串查：改了通配规则不影响命中精确规则的模型。
		{"override on other pattern ignored", &userRate, map[string]float64{"example-family-*": 0.9}, "example-family-small", 0.6, BillingRateSourceGroupModel},
		// 分组已删除的模式串上的残留覆盖不生效。
		{"stale override ignored", &userRate, map[string]float64{"removed-model": 0.1}, "removed-model", 0.5, BillingRateSourceUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, source := resolveBillingBaseRate(g, tc.userRate, tc.userModels, tc.model)
			require.Equal(t, tc.wantRate, rate)
			require.Equal(t, tc.wantSource, source)
		})
	}
}

func TestNormalizeGroupModelRateMultipliers(t *testing.T) {
	out, err := normalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{
		{Models: []string{"  model-a ", "model-b*"}, RateMultiplier: 0.7},
	})
	require.NoError(t, err)
	require.Equal(t, []GroupModelRateMultiplier{{Models: []string{"model-a", "model-b*"}, RateMultiplier: 0.7}}, out)

	empty, err := normalizeGroupModelRateMultipliers(nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	invalid := map[string][]GroupModelRateMultiplier{
		"zero rate":          {{Models: []string{"m"}, RateMultiplier: 0}},
		"negative rate":      {{Models: []string{"m"}, RateMultiplier: -1}},
		"nan rate":           {{Models: []string{"m"}, RateMultiplier: math.NaN()}},
		"inf rate":           {{Models: []string{"m"}, RateMultiplier: math.Inf(1)}},
		"no models":          {{Models: nil, RateMultiplier: 1}},
		"blank model":        {{Models: []string{"  "}, RateMultiplier: 1}},
		"bare wildcard":      {{Models: []string{"*"}, RateMultiplier: 1}},
		"inner wildcard":     {{Models: []string{"a*b"}, RateMultiplier: 1}},
		"duplicate in entry": {{Models: []string{"m", "M"}, RateMultiplier: 1}},
		"duplicate across": {
			{Models: []string{"m"}, RateMultiplier: 1},
			{Models: []string{"m"}, RateMultiplier: 2},
		},
	}
	for name, rules := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeGroupModelRateMultipliers(rules)
			require.Error(t, err)
		})
	}
}

func TestNormalizeUserGroupModelRates(t *testing.T) {
	g := modelRateTestGroup()

	out, err := normalizeUserGroupModelRates(g, map[string]float64{"EXAMPLE-OTHER": 0.4, "example-family-*": 0.95})
	require.NoError(t, err)
	require.Equal(t, map[string]float64{"Example-Other": 0.4, "example-family-*": 0.95}, out)

	empty, err := normalizeUserGroupModelRates(g, nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	_, err = normalizeUserGroupModelRates(g, map[string]float64{"not-in-group": 0.5})
	require.Error(t, err, "user layer may only modify rules that exist on the group")

	_, err = normalizeUserGroupModelRates(g, map[string]float64{"example-other": 0})
	require.Error(t, err)

	_, err = normalizeUserGroupModelRates(g, map[string]float64{"example-other": 0.5, "Example-Other": 0.6})
	require.Error(t, err)
}

type userGroupModelRateRepoStub struct {
	UserGroupRateRepository
	cfg   *UserGroupRateConfig
	err   error
	calls int
}

func (s *userGroupModelRateRepoStub) GetRateConfigByUserAndGroup(context.Context, int64, int64) (*UserGroupRateConfig, error) {
	s.calls++
	return s.cfg, s.err
}

func (s *userGroupModelRateRepoStub) SetUserGroupModelRates(context.Context, int64, int64, map[string]float64) error {
	return nil
}

func (s *userGroupModelRateRepoStub) PruneGroupModelRates(context.Context, int64, []string) error {
	return nil
}

func TestUserGroupRateResolverResolveForModel(t *testing.T) {
	userRate := 0.5
	repo := &userGroupModelRateRepoStub{cfg: &UserGroupRateConfig{
		RateMultiplier:       &userRate,
		ModelRateMultipliers: map[string]float64{"example-family-small": 0.45},
	}}
	resolver := newUserGroupRateResolver(repo, gocache.New(time.Minute, time.Minute), time.Minute, nil, "service.test")
	g := modelRateTestGroup()
	ctx := context.Background()

	rate, source := resolver.ResolveForModel(ctx, 101, g, "example-family-small", 1)
	require.Equal(t, 0.45, rate)
	require.Equal(t, BillingRateSourceUserModel, source)

	rate, source = resolver.ResolveForModel(ctx, 101, g, "example-family-large", 1)
	require.Equal(t, 1.0, rate)
	require.Equal(t, BillingRateSourceGroupModel, source)

	rate, source = resolver.ResolveForModel(ctx, 101, g, "unrelated-model", 1)
	require.Equal(t, 0.5, rate)
	require.Equal(t, BillingRateSourceUser, source)
	require.Equal(t, 1, repo.calls, "user config is cached per user:group, not per model")

	// 旧接口仍返回用户专属倍率，不看模型。
	require.Equal(t, 0.5, resolver.Resolve(ctx, 101, g.ID, g.RateMultiplier))

	// 缓存只存用户配置：分组规则变更立即生效，不等缓存过期。
	g.ModelRateMultipliers = nil
	rate, source = resolver.ResolveForModel(ctx, 101, g, "example-family-small", 1)
	require.Equal(t, 0.5, rate)
	require.Equal(t, BillingRateSourceUser, source)
	require.Equal(t, 1, repo.calls)

	rate, source = resolver.ResolveForModel(ctx, 101, nil, "example-family-small", 1.3)
	require.Equal(t, 1.3, rate)
	require.Equal(t, BillingRateSourceSystem, source)
}

func TestUserGroupRateResolverResolveForModel_NoRowAndErrors(t *testing.T) {
	g := modelRateTestGroup()
	ctx := context.Background()

	noRow := &userGroupModelRateRepoStub{}
	resolver := newUserGroupRateResolver(noRow, gocache.New(time.Minute, time.Minute), time.Minute, nil, "service.test")
	rate, source := resolver.ResolveForModel(ctx, 101, g, "unrelated-model", 1)
	require.Equal(t, 0.8, rate)
	require.Equal(t, BillingRateSourceGroup, source)
	_, _ = resolver.ResolveForModel(ctx, 101, g, "unrelated-model", 1)
	require.Equal(t, 1, noRow.calls, "absence of user config is cached too")

	failing := &userGroupModelRateRepoStub{err: context.DeadlineExceeded}
	resolver = newUserGroupRateResolver(failing, gocache.New(time.Minute, time.Minute), time.Minute, nil, "service.test")
	rate, source = resolver.ResolveForModel(ctx, 101, g, "example-family-small", 1)
	require.Equal(t, 0.6, rate, "read failure falls back to group-level resolution")
	require.Equal(t, BillingRateSourceGroupModel, source)

	// 仓储未实现模型覆盖能力时只按用户专属倍率解析。
	userRate := 0.5
	legacy := &userGroupRateResolverRepoStub{rate: &userRate}
	resolver = newUserGroupRateResolver(legacy, gocache.New(time.Minute, time.Minute), time.Minute, nil, "service.test")
	rate, source = resolver.ResolveForModel(ctx, 101, g, "example-family-small", 1)
	require.Equal(t, 0.6, rate)
	require.Equal(t, BillingRateSourceGroupModel, source)
	rate, source = resolver.ResolveForModel(ctx, 101, g, "unrelated-model", 1)
	require.Equal(t, 0.5, rate)
	require.Equal(t, BillingRateSourceUser, source)
}

// 预扣按每个候选计费模型各自的倍率估算再取最高：请求模型与渠道映射模型落在不同倍率上时，
// 预扣不得低于任一候选的实扣。
func TestInflightEstimate_UsesPerCandidateModelRate(t *testing.T) {
	groupID := int64(12)
	ch := Channel{
		ID:       1,
		Status:   StatusActive,
		GroupIDs: []int64{groupID},
		ModelMapping: map[string]map[string]string{
			"anthropic": {"claude-sonnet-4-5": "claude-haiku-4-5"},
		},
	}
	cs := newTestChannelService(makeStandardRepo(ch, map[int64]string{groupID: "anthropic"}))
	svc := newInflightEstimateGateway(t, cs)
	ctx := context.Background()
	req := InflightEstimateRequest{Model: "claude-sonnet-4-5", BodyBytes: 4000, MaxTokens: 1000}

	plain := &APIKey{User: &User{ID: 1}, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformAnthropic, RateMultiplier: 1}}
	base, ok := svc.EstimateInflightReservation(ctx, plain, req)
	require.True(t, ok)

	// 请求模型（较贵的候选）单独设 2 倍：估算必须翻倍。
	withRule := &APIKey{User: &User{ID: 1}, GroupID: &groupID, Group: &Group{
		ID: groupID, Platform: PlatformAnthropic, RateMultiplier: 1,
		ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"claude-sonnet-4-5"}, RateMultiplier: 2}},
	}}
	got, ok := svc.EstimateInflightReservation(ctx, withRule, req)
	require.True(t, ok)
	require.InDelta(t, base*2, got, 1e-12)

	// 只给较便宜的映射模型打折：较贵候选仍按分组默认倍率估算，预扣不变。
	discounted := &APIKey{User: &User{ID: 1}, GroupID: &groupID, Group: &Group{
		ID: groupID, Platform: PlatformAnthropic, RateMultiplier: 1,
		ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"claude-haiku-4-5"}, RateMultiplier: 0.1}},
	}}
	got, ok = svc.EstimateInflightReservation(ctx, discounted, req)
	require.True(t, ok)
	require.InDelta(t, base, got, 1e-12)
}
