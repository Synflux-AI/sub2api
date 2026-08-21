//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

type UserRepoAPIKeyGroupFilterSuite struct {
	suite.Suite
	ctx    context.Context
	client *dbent.Client
	repo   *userRepository
}

func (s *UserRepoAPIKeyGroupFilterSuite) SetupTest() {
	s.ctx = context.Background()
	s.client = testEntClient(s.T())
	s.repo = newUserRepositoryWithSQL(s.client, integrationDB)
	// api_keys 必须先于 users 清理（外键）；groups 也清理避免跨用例串扰。
	_, _ = integrationDB.ExecContext(s.ctx, "DELETE FROM api_keys")
	_, _ = integrationDB.ExecContext(s.ctx, "DELETE FROM user_allowed_groups")
	_, _ = integrationDB.ExecContext(s.ctx, "DELETE FROM user_subscriptions")
	_, _ = integrationDB.ExecContext(s.ctx, "DELETE FROM users")
	_, _ = integrationDB.ExecContext(s.ctx, "DELETE FROM groups")
}

func TestUserRepoAPIKeyGroupFilterSuite(t *testing.T) {
	suite.Run(t, new(UserRepoAPIKeyGroupFilterSuite))
}

func (s *UserRepoAPIKeyGroupFilterSuite) mustCreateUser(email string) *service.User {
	s.T().Helper()
	u := &service.User{
		Email:        email,
		PasswordHash: "test-password-hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Concurrency:  5,
	}
	s.Require().NoError(s.repo.Create(s.ctx, u), "create user")
	return u
}

func (s *UserRepoAPIKeyGroupFilterSuite) mustCreateGroup(name string) *dbent.Group {
	s.T().Helper()
	g, err := s.client.Group.Create().
		SetName(name).
		SetStatus(service.StatusActive).
		Save(s.ctx)
	s.Require().NoError(err, "create group")
	return g
}

func (s *UserRepoAPIKeyGroupFilterSuite) mustCreateAPIKey(userID int64, key, name string, groupID *int64) *dbent.APIKey {
	s.T().Helper()
	create := s.client.APIKey.Create().
		SetUserID(userID).
		SetKey(key).
		SetName(name)
	if groupID != nil {
		create = create.SetGroupID(*groupID)
	}
	ak, err := create.Save(s.ctx)
	s.Require().NoError(err, "create api key")
	return ak
}

func (s *UserRepoAPIKeyGroupFilterSuite) ids(users []service.User) []int64 {
	out := make([]int64, len(users))
	for i := range users {
		out[i] = users[i].ID
	}
	return out
}

func (s *UserRepoAPIKeyGroupFilterSuite) listByAPIKeyGroup(groupID int64) []service.User {
	s.T().Helper()
	users, _, err := s.repo.ListWithFilters(
		s.ctx,
		pagination.PaginationParams{Page: 1, PageSize: 50},
		service.UserListFilters{APIKeyGroupID: groupID},
	)
	s.Require().NoError(err, "ListWithFilters")
	return users
}

// 命中：拥有绑定到该分组 API Key 的用户出现，绑定到其它分组的不出现。
func (s *UserRepoAPIKeyGroupFilterSuite) TestFiltersUsersByAPIKeyGroup() {
	g := s.mustCreateGroup("grp-target")
	other := s.mustCreateGroup("grp-other")
	hit := s.mustCreateUser("hit@test.com")
	miss := s.mustCreateUser("miss@test.com")
	s.mustCreateAPIKey(hit.ID, "sk-hit", "K", &g.ID)
	s.mustCreateAPIKey(miss.ID, "sk-miss", "K", &other.ID)

	s.Require().Equal([]int64{hit.ID}, s.ids(s.listByAPIKeyGroup(g.ID)))
}

// 软删除的 API Key 不应命中（核心：软删除不会自动下沉到子查询，靠 DeletedAtIsNil 排除）。
func (s *UserRepoAPIKeyGroupFilterSuite) TestSoftDeletedAPIKeyExcluded() {
	g := s.mustCreateGroup("grp-soft")
	u := s.mustCreateUser("soft@test.com")
	ak := s.mustCreateAPIKey(u.ID, "sk-soft", "K", &g.ID)
	// 软删除该 key：SoftDeleteMixin 的 Hook 把 Delete 转为 UPDATE deleted_at。
	s.Require().NoError(s.client.APIKey.DeleteOne(ak).Exec(s.ctx), "soft delete api key")

	s.Require().Empty(s.listByAPIKeyGroup(g.ID), "user with only a soft-deleted key must not match")
}

// 多 Key：用户有多个 key，仅一个绑该分组 → 命中且只返回一条（EXISTS/去重）。
func (s *UserRepoAPIKeyGroupFilterSuite) TestMultipleKeysAnyMatchDedup() {
	g := s.mustCreateGroup("grp-multi")
	other := s.mustCreateGroup("grp-multi-other")
	u := s.mustCreateUser("multi@test.com")
	s.mustCreateAPIKey(u.ID, "sk-m1", "K1", &other.ID)
	s.mustCreateAPIKey(u.ID, "sk-m2", "K2", &g.ID)
	s.mustCreateAPIKey(u.ID, "sk-m3", "K3", nil) // 无分组

	s.Require().Equal([]int64{u.ID}, s.ids(s.listByAPIKeyGroup(g.ID)))
}

// 叠加过滤：api_key_group_id 与 status 同时指定时取交集——只返回同时满足两者的用户。
func (s *UserRepoAPIKeyGroupFilterSuite) TestAPIKeyGroupAndStatusFilter() {
	g := s.mustCreateGroup("grp-combined")

	// active 用户，key 绑 target 分组 → 应命中
	active := s.mustCreateUser("active-hit@test.com")
	s.mustCreateAPIKey(active.ID, "sk-active", "K", &g.ID)

	// disabled 用户，key 也绑 target 分组 → 只用 group 过滤会命中，但 status=active 后排除
	disabled := s.mustCreateUser("disabled-hit@test.com")
	s.mustCreateAPIKey(disabled.ID, "sk-disabled", "K2", &g.ID)
	_, err := s.client.User.UpdateOneID(disabled.ID).SetStatus(service.StatusDisabled).Save(s.ctx)
	s.Require().NoError(err, "disable user")

	// active 用户，key 绑其它分组 → group 过滤排除
	other := s.mustCreateGroup("grp-combined-other")
	miss := s.mustCreateUser("active-miss@test.com")
	s.mustCreateAPIKey(miss.ID, "sk-miss", "K3", &other.ID)

	users, _, err := s.repo.ListWithFilters(
		s.ctx,
		pagination.PaginationParams{Page: 1, PageSize: 50},
		service.UserListFilters{
			APIKeyGroupID: g.ID,
			Status:        service.StatusActive,
		},
	)
	s.Require().NoError(err)
	s.Require().Equal([]int64{active.ID}, s.ids(users), "only active user with matching key group should match")
}

// issue #171：Key 通过 api_key_groups **非默认**绑定到该分组的用户同样必须被筛出来。
// 改动前这里只查 api_keys.group_id 单列，会把这类用户漏掉。
func (s *UserRepoAPIKeyGroupFilterSuite) TestNonDefaultBoundGroupMatches() {
	target := s.mustCreateGroupWithPlatform("grp-nondefault-target", service.PlatformAnthropic)
	other := s.mustCreateGroupWithPlatform("grp-nondefault-other", service.PlatformOpenAI)

	// hit 的 Key 默认组是 other（openai），把 target（anthropic）作为非默认绑定。
	hit := s.mustCreateUser("nondefault-hit@test.com")
	hitKey := s.mustCreateAPIKey(hit.ID, "sk-nondefault-hit", "K", &other.ID)
	s.mustBindGroup(hitKey.ID, other.ID, service.PlatformOpenAI)
	s.mustBindGroup(hitKey.ID, target.ID, service.PlatformAnthropic)

	// miss 只绑 other，不该命中。
	miss := s.mustCreateUser("nondefault-miss@test.com")
	missKey := s.mustCreateAPIKey(miss.ID, "sk-nondefault-miss", "K", &other.ID)
	s.mustBindGroup(missKey.ID, other.ID, service.PlatformOpenAI)

	s.Require().Equal([]int64{hit.ID}, s.ids(s.listByAPIKeyGroup(target.ID)),
		"非默认绑定该分组的用户必须命中")
}

// 分组被软删后，该筛选（用户可见）不应再命中 —— 与 api_key 列表筛选的语义保持一致。
func (s *UserRepoAPIKeyGroupFilterSuite) TestSoftDeletedGroupDoesNotMatch() {
	target := s.mustCreateGroupWithPlatform("grp-softdeleted-target", service.PlatformAnthropic)
	u := s.mustCreateUser("softdeleted-group@test.com")
	ak := s.mustCreateAPIKey(u.ID, "sk-softdeleted-group", "K", &target.ID)
	s.mustBindGroup(ak.ID, target.ID, service.PlatformAnthropic)

	s.Require().NoError(s.client.Group.DeleteOneID(target.ID).Exec(s.ctx), "soft delete group")

	s.Require().Empty(s.listByAPIKeyGroup(target.ID),
		"用户可见筛选必须尊重分组软删")
}

func (s *UserRepoAPIKeyGroupFilterSuite) mustCreateGroupWithPlatform(name, platform string) *dbent.Group {
	s.T().Helper()
	g, err := s.client.Group.Create().
		SetName(name).
		SetPlatform(platform).
		SetStatus(service.StatusActive).
		Save(s.ctx)
	s.Require().NoError(err, "create group")
	return g
}

func (s *UserRepoAPIKeyGroupFilterSuite) mustBindGroup(apiKeyID, groupID int64, platform string) {
	s.T().Helper()
	err := s.client.APIKeyGroup.Create().
		SetAPIKeyID(apiKeyID).
		SetGroupID(groupID).
		SetPlatform(platform).
		Exec(s.ctx)
	s.Require().NoError(err, "bind group")
}

// 缺省（APIKeyGroupID=0）不过滤：所有用户都返回。
func (s *UserRepoAPIKeyGroupFilterSuite) TestZeroGroupIDNoFilter() {
	g := s.mustCreateGroup("grp-zero")
	u1 := s.mustCreateUser("z1@test.com")
	u2 := s.mustCreateUser("z2@test.com")
	s.mustCreateAPIKey(u1.ID, "sk-z1", "K", &g.ID)

	users, _, err := s.repo.ListWithFilters(
		s.ctx,
		pagination.PaginationParams{Page: 1, PageSize: 50},
		service.UserListFilters{APIKeyGroupID: 0},
	)
	s.Require().NoError(err)
	s.Require().ElementsMatch([]int64{u1.ID, u2.ID}, s.ids(users))
}
