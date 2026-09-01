//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// modelRPMRuleRepoStub 内存实现，够 resolver / CRUD 测试用。
type modelRPMRuleRepoStub struct {
	rules    []ModelRPMRule
	listErr  error
	nextID   int64
	listHits int
}

func (s *modelRPMRuleRepoStub) ListAll(context.Context) ([]ModelRPMRule, error) {
	s.listHits++
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]ModelRPMRule, len(s.rules))
	copy(out, s.rules)
	return out, nil
}

func (s *modelRPMRuleRepoStub) GetByID(_ context.Context, id int64) (*ModelRPMRule, error) {
	for i := range s.rules {
		if s.rules[i].ID == id {
			rule := s.rules[i]
			return &rule, nil
		}
	}
	return nil, ErrModelRPMRuleNotFound
}

func (s *modelRPMRuleRepoStub) Create(_ context.Context, rule *ModelRPMRule) error {
	s.nextID++
	rule.ID = s.nextID
	s.rules = append(s.rules, *rule)
	return nil
}

func (s *modelRPMRuleRepoStub) Update(_ context.Context, rule *ModelRPMRule) error {
	for i := range s.rules {
		if s.rules[i].ID == rule.ID {
			s.rules[i] = *rule
			return nil
		}
	}
	return ErrModelRPMRuleNotFound
}

func (s *modelRPMRuleRepoStub) Delete(_ context.Context, id int64) error {
	for i := range s.rules {
		if s.rules[i].ID == id {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			return nil
		}
	}
	return ErrModelRPMRuleNotFound
}

func TestNormalizeAndValidateModelRPMRule(t *testing.T) {
	targetID := int64(7)

	t.Run("normalizes name pattern and defaults", func(t *testing.T) {
		rule, err := NormalizeAndValidateModelRPMRule(&SaveModelRPMRuleInput{
			Name:         "  opus 限流  ",
			ModelPattern: "  Claude-Opus-*  ",
			RPMLimit:     10,
			Enabled:      true,
		})
		require.NoError(t, err)
		require.Equal(t, "opus 限流", rule.Name)
		require.Equal(t, "claude-opus-*", rule.ModelPattern)
		require.Equal(t, ModelRPMScopeUser, rule.Scope, "scope 缺省为 user")
		require.Equal(t, ModelRPMTargetAll, rule.TargetType, "target_type 缺省为 all")
		require.Nil(t, rule.TargetID)
	})

	t.Run("target_type=all drops target_id", func(t *testing.T) {
		rule, err := NormalizeAndValidateModelRPMRule(&SaveModelRPMRuleInput{
			Name: "n", ModelPattern: "m", TargetType: ModelRPMTargetAll, TargetID: &targetID, RPMLimit: 1,
		})
		require.NoError(t, err)
		require.Nil(t, rule.TargetID, "与库上的 CHECK 约束保持一致")
	})

	invalid := []struct {
		name  string
		input SaveModelRPMRuleInput
		want  error
	}{
		{"empty name", SaveModelRPMRuleInput{ModelPattern: "m", RPMLimit: 1}, ErrModelRPMRuleName},
		{"empty pattern", SaveModelRPMRuleInput{Name: "n", ModelPattern: "  ", RPMLimit: 1}, ErrModelRPMRulePattern},
		{"infix wildcard", SaveModelRPMRuleInput{Name: "n", ModelPattern: "cla*de", RPMLimit: 1}, ErrModelRPMRulePattern},
		{"bare wildcard", SaveModelRPMRuleInput{Name: "n", ModelPattern: "*", RPMLimit: 1}, ErrModelRPMRulePattern},
		{"bad scope", SaveModelRPMRuleInput{Name: "n", ModelPattern: "m", Scope: "account", RPMLimit: 1}, ErrModelRPMRuleScope},
		{"bad target type", SaveModelRPMRuleInput{Name: "n", ModelPattern: "m", TargetType: "api_key", RPMLimit: 1}, ErrModelRPMRuleTargetType},
		{"group without target id", SaveModelRPMRuleInput{Name: "n", ModelPattern: "m", TargetType: ModelRPMTargetGroup, RPMLimit: 1}, ErrModelRPMRuleTargetID},
		{"zero limit is not a green light", SaveModelRPMRuleInput{Name: "n", ModelPattern: "m", RPMLimit: 0}, ErrModelRPMRuleLimit},
		{"negative limit", SaveModelRPMRuleInput{Name: "n", ModelPattern: "m", RPMLimit: -1}, ErrModelRPMRuleLimit},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.input
			_, err := NormalizeAndValidateModelRPMRule(&input)
			require.ErrorIs(t, err, tc.want)
		})
	}

	t.Run("nil input", func(t *testing.T) {
		_, err := NormalizeAndValidateModelRPMRule(nil)
		require.ErrorIs(t, err, ErrModelRPMRuleNilInput)
	})
}

func TestModelRPMRuleMatching(t *testing.T) {
	exact := ModelRPMRule{ModelPattern: "claude-opus-4"}
	require.True(t, exact.MatchesModel("claude-opus-4"))
	require.False(t, exact.MatchesModel("claude-opus-4-1"))
	require.False(t, exact.MatchesModel(""))

	prefix := ModelRPMRule{ModelPattern: "claude-opus-*"}
	require.True(t, prefix.MatchesModel("claude-opus-4"))
	require.True(t, prefix.MatchesModel("claude-opus-4-20250101[1m]"))
	require.False(t, prefix.MatchesModel("claude-sonnet-4"))

	groupID := int64(3)
	groupRule := ModelRPMRule{TargetType: ModelRPMTargetGroup, TargetID: &groupID}
	require.True(t, groupRule.MatchesTarget(1, &Group{ID: 3}))
	require.False(t, groupRule.MatchesTarget(1, &Group{ID: 4}))
	require.False(t, groupRule.MatchesTarget(1, nil))

	userRule := ModelRPMRule{TargetType: ModelRPMTargetUser, TargetID: &groupID}
	require.True(t, userRule.MatchesTarget(3, nil))
	require.False(t, userRule.MatchesTarget(4, nil))

	allRule := ModelRPMRule{TargetType: ModelRPMTargetAll}
	require.True(t, allRule.MatchesTarget(0, nil))
}

func TestModelRPMRuleResolver_SnapshotFiltersDisabledAndSorts(t *testing.T) {
	groupID := int64(2)
	userID := int64(3)
	repo := &modelRPMRuleRepoStub{rules: []ModelRPMRule{
		{ID: 1, ModelPattern: "a", TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true},
		{ID: 2, ModelPattern: "b", TargetType: ModelRPMTargetGroup, TargetID: &groupID, RPMLimit: 1, Enabled: true},
		{ID: 3, ModelPattern: "c", TargetType: ModelRPMTargetUser, TargetID: &userID, RPMLimit: 1, Enabled: true},
		{ID: 4, ModelPattern: "d", TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: false},
		{ID: 5, ModelPattern: "e", TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true},
	}}
	resolver := NewModelRPMRuleResolver(repo)

	snapshot := resolver.Snapshot(context.Background())
	ids := make([]int64, 0, len(snapshot))
	for _, rule := range snapshot {
		ids = append(ids, rule.ID)
	}
	// user(3) > group(2) > all(1,5)，同具体度按 id 升序；停用的 4 被过滤。
	require.Equal(t, []int64{3, 2, 1, 5}, ids)

	// 第二次读走内存快照，不再打 DB。
	resolver.Snapshot(context.Background())
	require.Equal(t, 1, repo.listHits)

	// 管理端写入后主动失效，下一次读重新加载。
	resolver.Invalidate()
	resolver.Snapshot(context.Background())
	require.Equal(t, 2, repo.listHits)
}

func TestModelRPMRuleResolver_LoadErrorReusesLastSnapshot(t *testing.T) {
	repo := &modelRPMRuleRepoStub{rules: []ModelRPMRule{
		{ID: 1, ModelPattern: "a", TargetType: ModelRPMTargetAll, RPMLimit: 1, Enabled: true},
	}}
	resolver := NewModelRPMRuleResolver(repo)
	require.Len(t, resolver.Snapshot(context.Background()), 1)

	repo.listErr = errors.New("db down")
	resolver.Invalidate()

	// DB 抖动时继续用旧快照，而不是退化成「零规则 = 不限流」。
	require.Len(t, resolver.Snapshot(context.Background()), 1)
}

func TestModelRPMRuleResolver_LoadErrorWithoutSnapshotReturnsNil(t *testing.T) {
	repo := &modelRPMRuleRepoStub{listErr: errors.New("db down")}
	resolver := NewModelRPMRuleResolver(repo)
	require.Nil(t, resolver.Snapshot(context.Background()))
}

// modelRPMGroupRepoStub / modelRPMUserRepoStub 只实现 target 校验用到的方法。
type modelRPMGroupRepoStub struct {
	GroupRepository
	group *Group
	err   error
}

func (s *modelRPMGroupRepoStub) GetByIDLite(context.Context, int64) (*Group, error) {
	return s.group, s.err
}

type modelRPMUserRepoStub struct {
	UserRepository
	user *User
	err  error
}

func (s *modelRPMUserRepoStub) GetByID(context.Context, int64) (*User, error) {
	return s.user, s.err
}

func TestModelRPMRuleService_CreateValidatesTargetAndInvalidatesSnapshot(t *testing.T) {
	repo := &modelRPMRuleRepoStub{}
	resolver := NewModelRPMRuleResolver(repo)
	groupRepo := &modelRPMGroupRepoStub{group: &Group{ID: 4, Name: "vip"}}
	svc := NewModelRPMRuleService(repo, resolver, groupRepo, &modelRPMUserRepoStub{})
	ctx := context.Background()

	// 预热快照，随后建规则应让它失效。
	require.Empty(t, resolver.Snapshot(ctx))
	require.Equal(t, 1, repo.listHits)

	targetID := int64(4)
	created, err := svc.Create(ctx, &SaveModelRPMRuleInput{
		Name: "vip opus", ModelPattern: "claude-opus-*", Scope: ModelRPMScopeUser,
		TargetType: ModelRPMTargetGroup, TargetID: &targetID, RPMLimit: 5, Enabled: true,
	})
	require.NoError(t, err)
	require.Equal(t, "vip", created.TargetName)

	require.Len(t, resolver.Snapshot(ctx), 1, "写入后快照应立即失效并重新加载")
	require.Equal(t, 2, repo.listHits)
}

func TestModelRPMRuleService_CreateRejectsMissingTarget(t *testing.T) {
	repo := &modelRPMRuleRepoStub{}
	svc := NewModelRPMRuleService(repo, NewModelRPMRuleResolver(repo),
		&modelRPMGroupRepoStub{err: ErrGroupNotFound}, &modelRPMUserRepoStub{})

	targetID := int64(404)
	_, err := svc.Create(context.Background(), &SaveModelRPMRuleInput{
		Name: "ghost", ModelPattern: "m", TargetType: ModelRPMTargetGroup, TargetID: &targetID, RPMLimit: 1,
	})
	require.Error(t, err, "目标不存在的规则永不命中，属于静默失效，应直接拒绝")
	require.Empty(t, repo.rules)
}

func TestModelRPMRuleService_UpdateAndDelete(t *testing.T) {
	repo := &modelRPMRuleRepoStub{}
	resolver := NewModelRPMRuleResolver(repo)
	svc := NewModelRPMRuleService(repo, resolver, &modelRPMGroupRepoStub{}, &modelRPMUserRepoStub{})
	ctx := context.Background()

	created, err := svc.Create(ctx, &SaveModelRPMRuleInput{
		Name: "n", ModelPattern: "m", RPMLimit: 1, Enabled: true,
	})
	require.NoError(t, err)

	updated, err := svc.Update(ctx, created.ID, &SaveModelRPMRuleInput{
		Name: "n2", ModelPattern: "m2", Scope: ModelRPMScopeGlobal, RPMLimit: 9, Enabled: false,
	})
	require.NoError(t, err)
	require.Equal(t, created.ID, updated.ID)
	require.Equal(t, "m2", updated.ModelPattern)
	require.Equal(t, ModelRPMScopeGlobal, updated.Scope)
	require.Equal(t, 9, updated.RPMLimit)
	require.False(t, updated.Enabled)

	_, err = svc.Update(ctx, 999, &SaveModelRPMRuleInput{Name: "n", ModelPattern: "m", RPMLimit: 1})
	require.ErrorIs(t, err, ErrModelRPMRuleNotFound)

	require.NoError(t, svc.Delete(ctx, created.ID))
	require.ErrorIs(t, svc.Delete(ctx, created.ID), ErrModelRPMRuleNotFound)
}
