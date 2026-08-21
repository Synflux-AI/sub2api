//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// GroupRepoAPIKeyBindingsSuite 覆盖 issue #171 里「分组侧改动要连带维护 api_key_groups」
// 的两条路径：软删分组、变更分组 platform。
//
// 这两条路径不修就会留下两类隐患：
//   - 软删分组不清绑定行 → DB 的 UNIQUE (api_key_id, platform) 不认软删，
//     之后给同一把 Key 绑一个在用的同平台分组会吃 Postgres 23505；
//   - 改 platform 不同步绑定行 → 选组按 api_key_groups.platform 匹配，
//     不同步就会「按旧平台选组」，或者产生同平台双绑而让计费取决于排序。
type GroupRepoAPIKeyBindingsSuite struct {
	suite.Suite
	ctx        context.Context
	client     *dbent.Client
	groupRepo  *groupRepository
	apiKeyRepo *apiKeyRepository
	seq        int
}

func (s *GroupRepoAPIKeyBindingsSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.groupRepo = newGroupRepositoryWithSQL(s.client, tx)
	s.apiKeyRepo = newAPIKeyRepositoryWithSQL(s.client, tx)
}

func TestGroupRepoAPIKeyBindingsSuite(t *testing.T) {
	suite.Run(t, new(GroupRepoAPIKeyBindingsSuite))
}

func (s *GroupRepoAPIKeyBindingsSuite) uniq(prefix string) string {
	s.seq++
	return fmt.Sprintf("gbind-%s-%d-%d", prefix, time.Now().UnixNano(), s.seq)
}

func (s *GroupRepoAPIKeyBindingsSuite) mustUser() *service.User {
	s.T().Helper()
	u, err := s.client.User.Create().
		SetEmail(s.uniq("user") + "@test.com").
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(s.ctx)
	s.Require().NoError(err)
	return userEntityToService(u)
}

func (s *GroupRepoAPIKeyBindingsSuite) mustGroup(platform string) *service.Group {
	s.T().Helper()
	g, err := s.client.Group.Create().
		SetName(s.uniq("group-" + platform)).
		SetPlatform(platform).
		SetStatus(service.StatusActive).
		Save(s.ctx)
	s.Require().NoError(err)
	return groupEntityToService(g)
}

func (s *GroupRepoAPIKeyBindingsSuite) mustKey(userID int64, defaultGroup *service.Group, bound ...*service.Group) *service.APIKey {
	s.T().Helper()
	key := &service.APIKey{
		UserID:      userID,
		Key:         "sk-" + s.uniq("key"),
		Name:        s.uniq("name"),
		Status:      service.StatusActive,
		BoundGroups: bound,
	}
	if defaultGroup != nil {
		gid := defaultGroup.ID
		key.GroupID = &gid
	}
	s.Require().NoError(s.apiKeyRepo.Create(s.ctx, key))
	return key
}

func (s *GroupRepoAPIKeyBindingsSuite) boundIDs(apiKeyID int64) []int64 {
	s.T().Helper()
	ids, err := s.apiKeyRepo.ListBoundGroupIDs(s.ctx, apiKeyID)
	s.Require().NoError(err)
	return ids
}

func (s *GroupRepoAPIKeyBindingsSuite) defaultGroupID(apiKeyID int64) *int64 {
	s.T().Helper()
	key, err := s.apiKeyRepo.GetByID(s.ctx, apiKeyID)
	s.Require().NoError(err)
	return key.GroupID
}

// --- 软删分组：清理绑定行 + 改选默认组 ---

func (s *GroupRepoAPIKeyBindingsSuite) TestDeleteCascadeRemovesBindingsAndReselectsDefaultGroup() {
	user := s.mustUser()
	anthropic := s.mustGroup(service.PlatformAnthropic)
	openai := s.mustGroup(service.PlatformOpenAI)
	gemini := s.mustGroup(service.PlatformGemini)

	// 默认组是 anthropic（三个平台里 platform 字典序最小的那个）。
	key := s.mustKey(user.ID, anthropic, anthropic, openai, gemini)
	s.Require().ElementsMatch([]int64{anthropic.ID, openai.ID, gemini.ID}, s.boundIDs(key.ID))

	_, err := s.groupRepo.DeleteCascade(s.ctx, anthropic.ID)
	s.Require().NoError(err)

	// 绑定行必须被清掉 —— 否则残留行会让之后绑一个在用的 anthropic 分组吃 23505。
	s.Require().ElementsMatch([]int64{openai.ID, gemini.ID}, s.boundIDs(key.ID),
		"删掉的分组的绑定行必须被清理（软删不触发 FK CASCADE）")

	// 默认组必须从剩余绑定里按 (platform, id) 规则改选。
	// 剩余是 gemini + openai，platform 字典序 gemini < openai。
	got := s.defaultGroupID(key.ID)
	s.Require().NotNil(got, "还有剩余绑定时不应把默认组置空")
	s.Require().EqualValues(gemini.ID, *got,
		"默认组改选必须与 service.ResolveDefaultGroupID 同规则：platform 字典序优先")
}

// 一个绑定都不剩时**必须保留**原 group_id —— 这是早于本 issue 的、有意为之的不变量。
//
// 保留它，认证才会给出 403 GROUP_DELETED「API Key 所属分组已删除」。
// 置空会让这把 Key 变成「未分组」，而未分组在 IsUngroupedKeySchedulingAllowed
// 为真时是**放行**的 —— 等于把一次明确拒绝变成潜在放行。
// 参见 allowed_groups_contract_integration_test.go 的
// TestGroupRepository_DeleteCascade_PreservesApiKeyGroupID。
func (s *GroupRepoAPIKeyBindingsSuite) TestDeleteCascadeKeepsDanglingDefaultGroupWhenNoBindingLeft() {
	user := s.mustUser()
	only := s.mustGroup(service.PlatformAnthropic)
	key := s.mustKey(user.ID, only, only)

	_, err := s.groupRepo.DeleteCascade(s.ctx, only.ID)
	s.Require().NoError(err)

	// 绑定行仍然要清掉（防止残留行撞 UNIQUE (api_key_id, platform)）……
	s.Require().Empty(s.boundIDs(key.ID))
	// ……但 group_id 必须保持指向已删分组，让认证继续以 GROUP_DELETED 拒绝。
	got := s.defaultGroupID(key.ID)
	s.Require().NotNil(got,
		"没有剩余绑定时不得把 group_id 置空：那会把「分组已删除」的拒绝变成「未分组」的潜在放行")
	s.Require().EqualValues(only.ID, *got)
}

// 删掉的不是默认组时，默认组必须保持原样 —— 不能顺手改动。
func (s *GroupRepoAPIKeyBindingsSuite) TestDeleteCascadeKeepsDefaultGroupWhenDeletingNonDefault() {
	user := s.mustUser()
	anthropic := s.mustGroup(service.PlatformAnthropic)
	openai := s.mustGroup(service.PlatformOpenAI)
	key := s.mustKey(user.ID, anthropic, anthropic, openai)

	_, err := s.groupRepo.DeleteCascade(s.ctx, openai.ID)
	s.Require().NoError(err)

	s.Require().ElementsMatch([]int64{anthropic.ID}, s.boundIDs(key.ID))
	got := s.defaultGroupID(key.ID)
	s.Require().NotNil(got)
	s.Require().EqualValues(anthropic.ID, *got, "删非默认组不得改动默认组")
}

// 默认组改选的 SQL 表达（ORDER BY platform, group_id）必须与 Go 侧
// service.ResolveDefaultGroupID 完全一致。这两处是同一条规则的两种表达，
// 改任何一边都必须同步另一边 —— 本用例就是那道锁。
func (s *GroupRepoAPIKeyBindingsSuite) TestDeleteCascadeDefaultGroupReselectionMatchesResolveDefaultGroupID() {
	user := s.mustUser()
	doomed := s.mustGroup(service.PlatformAnthropic)
	// 刻意让 id 顺序与 platform 字典序相反，这样「按 id 选」和「按 platform 选」
	// 会给出不同答案，测试才有区分力。
	openai := s.mustGroup(service.PlatformOpenAI) // id 较小
	gemini := s.mustGroup(service.PlatformGemini) // id 较大，但 platform 字典序更小
	s.Require().Less(openai.ID, gemini.ID, "夹具前提：openai 的 id 必须更小")

	key := s.mustKey(user.ID, doomed, doomed, openai, gemini)
	_, err := s.groupRepo.DeleteCascade(s.ctx, doomed.ID)
	s.Require().NoError(err)

	wantPtr := service.ResolveDefaultGroupIDFromGroups([]*service.Group{openai, gemini})
	s.Require().NotNil(wantPtr)
	got := s.defaultGroupID(key.ID)
	s.Require().NotNil(got)
	s.Require().EqualValues(*wantPtr, *got,
		"SQL 的 ORDER BY platform, group_id 必须与 service.ResolveDefaultGroupID 同结果")
	s.Require().EqualValues(gemini.ID, *got, "两者都应选 platform 字典序更小的 gemini，而不是 id 更小的 openai")
}

// --- 变更分组 platform ---

func (s *GroupRepoAPIKeyBindingsSuite) TestUpdateSyncsBindingPlatform() {
	user := s.mustUser()
	g := s.mustGroup(service.PlatformAnthropic)
	key := s.mustKey(user.ID, g, g)

	g.Platform = service.PlatformGrok
	s.Require().NoError(s.groupRepo.Update(s.ctx, g))

	platform, found, err := scanOneString(s.ctx, s.client,
		"SELECT platform FROM api_key_groups WHERE api_key_id = $1 AND group_id = $2", key.ID, g.ID)
	s.Require().NoError(err)
	s.Require().True(found, "绑定行必须还在")
	s.Require().Equal(service.PlatformGrok, platform,
		"分组 platform 变更后，api_key_groups.platform 必须同步——选组是按它匹配的")
}

func (s *GroupRepoAPIKeyBindingsSuite) TestUpdateRejectsPlatformChangeThatWouldCreateSamePlatformConflict() {
	user := s.mustUser()
	anthropic := s.mustGroup(service.PlatformAnthropic)
	openai := s.mustGroup(service.PlatformOpenAI)
	key := s.mustKey(user.ID, anthropic, anthropic, openai)

	// 把 anthropic 组改成 openai —— 这把 Key 就会同平台绑两个组。
	anthropic.Platform = service.PlatformOpenAI
	err := s.groupRepo.Update(s.ctx, anthropic)
	s.Require().ErrorIs(err, service.ErrGroupPlatformChangeConflict)

	// 整体回滚：分组行与绑定行都不能有任何改动。
	current, found, scanErr := scanOneString(s.ctx, s.client,
		"SELECT platform FROM groups WHERE id = $1", anthropic.ID)
	s.Require().NoError(scanErr)
	s.Require().True(found)
	s.Require().Equal(service.PlatformAnthropic, current,
		"冲突时分组 platform 必须保持原值，不能留下「分组改了、绑定没改」的中间态")

	bindingPlatform, found, scanErr := scanOneString(s.ctx, s.client,
		"SELECT platform FROM api_key_groups WHERE api_key_id = $1 AND group_id = $2", key.ID, anthropic.ID)
	s.Require().NoError(scanErr)
	s.Require().True(found)
	s.Require().Equal(service.PlatformAnthropic, bindingPlatform)
}

// 冲突判定必须排除「同一个分组」自己：只绑了这一个组时，改它的 platform 不是冲突。
func (s *GroupRepoAPIKeyBindingsSuite) TestUpdateAllowsPlatformChangeWhenOnlyBoundToItself() {
	user := s.mustUser()
	g := s.mustGroup(service.PlatformAnthropic)
	s.mustKey(user.ID, g, g)

	g.Platform = service.PlatformOpenAI
	s.Require().NoError(s.groupRepo.Update(s.ctx, g),
		"只绑了这一个分组时，改它的 platform 不构成同平台冲突")
}

// platform 没变时不得对 api_key_groups 发任何写语句 —— 绝大多数 UpdateGroup
// 请求（改名、改倍率）都走这条路，不能因为多分组特性给它们加写开销。
func (s *GroupRepoAPIKeyBindingsSuite) TestUpdateWithoutPlatformChangeLeavesBindingsUntouched() {
	user := s.mustUser()
	g := s.mustGroup(service.PlatformAnthropic)
	key := s.mustKey(user.ID, g, g)

	before, found, err := scanOneString(s.ctx, s.client,
		"SELECT platform FROM api_key_groups WHERE api_key_id = $1 AND group_id = $2", key.ID, g.ID)
	s.Require().NoError(err)
	s.Require().True(found)

	g.Name = s.uniq("renamed")
	g.RateMultiplier = 2.5
	s.Require().NoError(s.groupRepo.Update(s.ctx, g))

	after, found, err := scanOneString(s.ctx, s.client,
		"SELECT platform FROM api_key_groups WHERE api_key_id = $1 AND group_id = $2", key.ID, g.ID)
	s.Require().NoError(err)
	s.Require().True(found)
	s.Require().Equal(before, after)
	s.Require().ElementsMatch([]int64{g.ID}, s.boundIDs(key.ID))
}
