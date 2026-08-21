//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// APIKeyBindingsSuite 覆盖 issue #171 的多分组绑定读写：
// ReplaceBindings 的整体替换语义、「默认组 UNION 关联表」反查、列表筛选语义、
// 以及 BoundGroups 的读路径回填。
//
// 所有用例跑在 testEntTx 的事务里，测试结束自动回滚。
type APIKeyBindingsSuite struct {
	suite.Suite
	ctx    context.Context
	client *dbent.Client
	repo   *apiKeyRepository
	seq    int
}

func (s *APIKeyBindingsSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.repo = newAPIKeyRepositoryWithSQL(s.client, tx)
}

func TestAPIKeyBindingsSuite(t *testing.T) {
	suite.Run(t, new(APIKeyBindingsSuite))
}

func (s *APIKeyBindingsSuite) uniq(prefix string) string {
	s.seq++
	return fmt.Sprintf("bindings-%s-%d-%d", prefix, time.Now().UnixNano(), s.seq)
}

func (s *APIKeyBindingsSuite) mustUser() *service.User {
	s.T().Helper()
	u, err := s.client.User.Create().
		SetEmail(s.uniq("user") + "@test.com").
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(s.ctx)
	s.Require().NoError(err, "create user")
	return userEntityToService(u)
}

func (s *APIKeyBindingsSuite) mustGroup(platform string) *service.Group {
	s.T().Helper()
	g, err := s.client.Group.Create().
		SetName(s.uniq("group-" + platform)).
		SetPlatform(platform).
		SetStatus(service.StatusActive).
		Save(s.ctx)
	s.Require().NoError(err, "create group")
	return groupEntityToService(g)
}

func (s *APIKeyBindingsSuite) mustSoftDeleteGroup(id int64) {
	s.T().Helper()
	s.Require().NoError(s.client.Group.DeleteOneID(id).Exec(s.ctx), "soft delete group")

	// 确认走的是软删（行还在，只是 deleted_at 有值），否则本用例的前提不成立。
	exists, err := s.client.Group.Query().Where(entgroup.IDEQ(id)).Exist(mixins.SkipSoftDelete(s.ctx))
	s.Require().NoError(err)
	s.Require().True(exists, "group row must still exist after soft delete")
}

func (s *APIKeyBindingsSuite) mustKey(userID int64, defaultGroup *service.Group, bound ...*service.Group) *service.APIKey {
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
	s.Require().NoError(s.repo.Create(s.ctx, key), "create api key")
	return key
}

// --- ReplaceBindings：整体替换 ---

func (s *APIKeyBindingsSuite) TestReplaceBindingsReplacesWholeSet() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)
	anthropicC := s.mustGroup(service.PlatformAnthropic)

	key := s.mustKey(user.ID, anthropicA, anthropicA)
	s.Require().Equal([]int64{anthropicA.ID}, s.mustBoundIDs(key.ID))

	// 追加一个不同平台的绑定。
	s.Require().NoError(s.repo.ReplaceBindings(s.ctx, key.ID, []service.GroupBinding{
		{GroupID: anthropicA.ID, Platform: service.PlatformAnthropic},
		{GroupID: openaiB.ID, Platform: service.PlatformOpenAI},
	}))
	s.Require().ElementsMatch([]int64{anthropicA.ID, openaiB.ID}, s.mustBoundIDs(key.ID))

	// 整体替换：旧的两条都要消失，只剩新集合。
	s.Require().NoError(s.repo.ReplaceBindings(s.ctx, key.ID, []service.GroupBinding{
		{GroupID: anthropicC.ID, Platform: service.PlatformAnthropic},
	}))
	s.Require().Equal([]int64{anthropicC.ID}, s.mustBoundIDs(key.ID))

	// 空集合清空全部绑定。
	s.Require().NoError(s.repo.ReplaceBindings(s.ctx, key.ID, nil))
	s.Require().Empty(s.mustBoundIDs(key.ID))
}

func (s *APIKeyBindingsSuite) TestReplaceBindingsIsScopedToOneKey() {
	user := s.mustUser()
	groupA := s.mustGroup(service.PlatformAnthropic)
	groupB := s.mustGroup(service.PlatformOpenAI)

	key1 := s.mustKey(user.ID, groupA, groupA)
	key2 := s.mustKey(user.ID, groupA, groupA)

	s.Require().NoError(s.repo.ReplaceBindings(s.ctx, key1.ID, []service.GroupBinding{
		{GroupID: groupB.ID, Platform: service.PlatformOpenAI},
	}))
	s.Require().Equal([]int64{groupB.ID}, s.mustBoundIDs(key1.ID))
	s.Require().Equal([]int64{groupA.ID}, s.mustBoundIDs(key2.ID), "不得影响其它 Key 的绑定")
}

// TestReplaceBindingsClearsResidualSoftDeletedGroupBinding 是本 Task 的核心回归：
//
// DB 上的 UNIQUE (api_key_id, platform) 不认软删，而迁移 230 的回填没有过滤已软删的分组，
// 所以生产库里可能残留「指向已软删分组的绑定行」。用例先实锤这一冲突确实存在（定点 INSERT 撞 23505），
// 再证明 ReplaceBindings 的整体替换语义能把残留行一并清掉，从而完全避开冲突。
func (s *APIKeyBindingsSuite) TestReplaceBindingsClearsResidualSoftDeletedGroupBinding() {
	user := s.mustUser()
	staleGroup := s.mustGroup(service.PlatformAnthropic)
	liveGroup := s.mustGroup(service.PlatformAnthropic)

	key := s.mustKey(user.ID, staleGroup, staleGroup)
	s.mustSoftDeleteGroup(staleGroup.ID)
	s.Require().Equal([]int64{staleGroup.ID}, s.mustBoundIDs(key.ID),
		"软删分组不会级联清理关联行，残留行必须还在（本用例的前提）")

	// 前提验证：不走 replace 而是定点插入同平台的在用分组，会撞唯一约束。
	// 唯一冲突会把整个测试事务打成 aborted，所以用 SAVEPOINT 隔离。
	_, err := s.client.ExecContext(s.ctx, "SAVEPOINT residual_conflict")
	s.Require().NoError(err)
	_, insertErr := s.client.ExecContext(s.ctx,
		`INSERT INTO api_key_groups (api_key_id, group_id, platform) VALUES ($1, $2, $3)`,
		key.ID, liveGroup.ID, service.PlatformAnthropic)
	s.Require().Error(insertErr, "定点插入必须撞上 idx_api_key_groups_key_platform")
	s.Require().Contains(insertErr.Error(), "idx_api_key_groups_key_platform")
	_, err = s.client.ExecContext(s.ctx, "ROLLBACK TO SAVEPOINT residual_conflict")
	s.Require().NoError(err)

	// 整体替换：先删光残留行再插入，不会有冲突。
	s.Require().NoError(s.repo.ReplaceBindings(s.ctx, key.ID, []service.GroupBinding{
		{GroupID: liveGroup.ID, Platform: service.PlatformAnthropic},
	}), "ReplaceBindings 必须清掉残留的软删分组绑定行，不能撞 23505")
	s.Require().Equal([]int64{liveGroup.ID}, s.mustBoundIDs(key.ID))
}

func (s *APIKeyBindingsSuite) TestReplaceBindingsRejectsSamePlatformTwice() {
	user := s.mustUser()
	groupA := s.mustGroup(service.PlatformAnthropic)
	groupB := s.mustGroup(service.PlatformAnthropic)
	key := s.mustKey(user.ID, nil)

	_, err := s.client.ExecContext(s.ctx, "SAVEPOINT same_platform")
	s.Require().NoError(err)
	err = s.repo.ReplaceBindings(s.ctx, key.ID, []service.GroupBinding{
		{GroupID: groupA.ID, Platform: service.PlatformAnthropic},
		{GroupID: groupB.ID, Platform: service.PlatformAnthropic},
	})
	s.Require().ErrorIs(err, service.ErrAPIKeyGroupBindingConflict)
	_, err = s.client.ExecContext(s.ctx, "ROLLBACK TO SAVEPOINT same_platform")
	s.Require().NoError(err)
}

// --- 反查：默认组 UNION 关联表（C12）---

func (s *APIKeyBindingsSuite) TestListKeysByGroupIDCoversNonDefaultBindings() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)

	// key1 只把 A 当默认组；key2 默认组是 B，把 A 作为非默认绑定；key3 完全没绑定。
	key1 := s.mustKey(user.ID, anthropicA, anthropicA)
	key2 := s.mustKey(user.ID, openaiB, openaiB, anthropicA)
	key3 := s.mustKey(user.ID, nil)

	keysForA, err := s.repo.ListKeysByGroupID(s.ctx, anthropicA.ID)
	s.Require().NoError(err)
	s.Require().ElementsMatch([]string{key1.Key, key2.Key}, keysForA,
		"非默认绑定的 Key 必须被反查到，否则分组改配后它的认证缓存不会失效")

	keysForB, err := s.repo.ListKeysByGroupID(s.ctx, openaiB.ID)
	s.Require().NoError(err)
	s.Require().ElementsMatch([]string{key2.Key}, keysForB)

	ids, err := s.repo.ListKeyIDsByBoundGroupID(s.ctx, anthropicA.ID)
	s.Require().NoError(err)
	s.Require().Equal([]int64{key1.ID, key2.ID}, ids, "ID 反查按 ID 升序")

	s.Require().NotContains(keysForA, key3.Key)
}

func (s *APIKeyBindingsSuite) TestListKeysByGroupIDExcludesSoftDeletedKeys() {
	user := s.mustUser()
	groupA := s.mustGroup(service.PlatformAnthropic)
	key1 := s.mustKey(user.ID, groupA, groupA)
	key2 := s.mustKey(user.ID, nil, groupA)

	s.Require().NoError(s.repo.Delete(s.ctx, key1.ID))

	keys, err := s.repo.ListKeysByGroupID(s.ctx, groupA.ID)
	s.Require().NoError(err)
	s.Require().ElementsMatch([]string{key2.Key}, keys)
}

func (s *APIKeyBindingsSuite) TestListKeysByGroupIDStillMatchesSoftDeletedGroup() {
	user := s.mustUser()
	groupA := s.mustGroup(service.PlatformAnthropic)
	key := s.mustKey(user.ID, nil, groupA)

	s.mustSoftDeleteGroup(groupA.ID)

	keys, err := s.repo.ListKeysByGroupID(s.ctx, groupA.ID)
	s.Require().NoError(err)
	s.Require().Equal([]string{key.Key}, keys,
		"分组被软删同样需要失效这些 Key 的认证缓存，反查不能 join groups 过滤掉")
}

// --- 列表筛选语义 ---

func (s *APIKeyBindingsSuite) TestListByUserIDFilterMatchesBindingSetMembership() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)

	key1 := s.mustKey(user.ID, anthropicA, anthropicA)
	key2 := s.mustKey(user.ID, openaiB, openaiB, anthropicA)
	key3 := s.mustKey(user.ID, nil)

	params := pagination.PaginationParams{Page: 1, PageSize: 50}

	byA := s.mustListKeyStrings(user.ID, params, anthropicA.ID)
	s.Require().ElementsMatch([]string{key1.Key, key2.Key}, byA,
		"按分组筛选应命中非默认绑定的 Key")

	byB := s.mustListKeyStrings(user.ID, params, openaiB.ID)
	s.Require().ElementsMatch([]string{key2.Key}, byB)

	unbound := s.mustListKeyStrings(user.ID, params, 0)
	s.Require().ElementsMatch([]string{key3.Key}, unbound, "0 表示未绑定任何分组")
}

func (s *APIKeyBindingsSuite) TestListByUserIDBackfillsBoundGroups() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)
	key := s.mustKey(user.ID, openaiB, openaiB, anthropicA)

	keys, _, err := s.repo.ListByUserID(s.ctx, user.ID,
		pagination.PaginationParams{Page: 1, PageSize: 50}, service.APIKeyListFilters{})
	s.Require().NoError(err)

	var found *service.APIKey
	for i := range keys {
		if keys[i].ID == key.ID {
			found = &keys[i]
		}
	}
	s.Require().NotNil(found)
	s.Require().Len(found.BoundGroups, 2)
	s.Require().Equal(anthropicA.ID, found.BoundGroups[0].ID, "先按 platform 字典序")
	s.Require().Equal(openaiB.ID, found.BoundGroups[1].ID)
	s.Require().NotNil(found.GroupID)
	s.Require().Equal(openaiB.ID, *found.GroupID, "GroupID 仍是默认组")
}

// --- 读路径回填 BoundGroups ---

func (s *APIKeyBindingsSuite) TestGetByIDAndGetByKeyBackfillBoundGroups() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)
	key := s.mustKey(user.ID, anthropicA, anthropicA, openaiB)

	byID, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Require().Len(byID.BoundGroups, 2)
	s.Require().Equal(anthropicA.ID, byID.BoundGroups[0].ID)
	s.Require().Equal(openaiB.ID, byID.BoundGroups[1].ID)
	s.Require().NotNil(byID.Group)
	s.Require().Equal(anthropicA.ID, byID.Group.ID)

	byKey, err := s.repo.GetByKey(s.ctx, key.Key)
	s.Require().NoError(err)
	s.Require().Len(byKey.BoundGroups, 2)
	s.Require().Equal(anthropicA.ID, byKey.BoundGroups[0].ID)
	s.Require().Equal(openaiB.ID, byKey.BoundGroups[1].ID)
}

func (s *APIKeyBindingsSuite) TestBoundGroupsExcludeSoftDeletedGroupsButRawListDoesNot() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)
	key := s.mustKey(user.ID, anthropicA, anthropicA, openaiB)

	s.mustSoftDeleteGroup(openaiB.ID)

	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Require().Len(got.BoundGroups, 1, "读路径必须剔除指向已软删分组的绑定")
	s.Require().Equal(anthropicA.ID, got.BoundGroups[0].ID)

	raw, err := s.repo.ListBoundGroupIDs(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Require().ElementsMatch([]int64{anthropicA.ID, openaiB.ID}, raw,
		"ListBoundGroupIDs 返回原始绑定集合，不过滤软删")
}

// --- Create / Update 的事务语义（成功路径）---

func (s *APIKeyBindingsSuite) TestCreateWritesMainRowAndBindings() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)

	key := s.mustKey(user.ID, anthropicA, anthropicA, openaiB)
	s.Require().NotZero(key.ID)
	s.Require().ElementsMatch([]int64{anthropicA.ID, openaiB.ID}, s.mustBoundIDs(key.ID))
}

func (s *APIKeyBindingsSuite) TestUpdateReplacesBindingsAndDefaultGroupTogether() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)
	key := s.mustKey(user.ID, anthropicA, anthropicA)

	key.GroupID = &openaiB.ID
	key.BoundGroups = []*service.Group{openaiB}
	s.Require().NoError(s.repo.Update(s.ctx, key,
		service.APIKeyUpdateFields{GroupID: true, BoundGroups: true}))

	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.GroupID)
	s.Require().Equal(openaiB.ID, *got.GroupID)
	s.Require().Equal([]int64{openaiB.ID}, s.mustBoundIDs(key.ID))
	s.Require().Len(got.BoundGroups, 1)
	s.Require().Equal(openaiB.ID, got.BoundGroups[0].ID)
}

func (s *APIKeyBindingsSuite) TestUpdateWithoutBoundGroupsMaskLeavesBindingsUntouched() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	key := s.mustKey(user.ID, anthropicA, anthropicA)

	key.Name = "renamed"
	s.Require().NoError(s.repo.Update(s.ctx, key, service.APIKeyUpdateFields{Name: true}))
	s.Require().Equal([]int64{anthropicA.ID}, s.mustBoundIDs(key.ID))
}

// --- DeleteBindingsByGroupID ---

func (s *APIKeyBindingsSuite) TestDeleteBindingsByGroupIDRemovesOnlyThatGroup() {
	user := s.mustUser()
	anthropicA := s.mustGroup(service.PlatformAnthropic)
	openaiB := s.mustGroup(service.PlatformOpenAI)

	key1 := s.mustKey(user.ID, anthropicA, anthropicA, openaiB)
	key2 := s.mustKey(user.ID, anthropicA, anthropicA)

	n, err := s.repo.DeleteBindingsByGroupID(s.ctx, anthropicA.ID)
	s.Require().NoError(err)
	s.Require().Equal(int64(2), n)

	s.Require().Equal([]int64{openaiB.ID}, s.mustBoundIDs(key1.ID))
	s.Require().Empty(s.mustBoundIDs(key2.ID))
}

func (s *APIKeyBindingsSuite) mustBoundIDs(apiKeyID int64) []int64 {
	s.T().Helper()
	ids, err := s.repo.ListBoundGroupIDs(s.ctx, apiKeyID)
	s.Require().NoError(err, "ListBoundGroupIDs")
	return ids
}

func (s *APIKeyBindingsSuite) mustListKeyStrings(userID int64, params pagination.PaginationParams, groupID int64) []string {
	s.T().Helper()
	keys, _, err := s.repo.ListByUserID(s.ctx, userID, params,
		service.APIKeyListFilters{GroupID: &groupID})
	s.Require().NoError(err, "ListByUserID")
	out := make([]string, 0, len(keys))
	for i := range keys {
		out = append(out, keys[i].Key)
	}
	return out
}

// --- 事务回滚：关联表写入失败时主表不留中间态 ---
//
// 这两个用例不能跑在 testEntTx 里：真实 Postgres 下失败语句会把外层测试事务打成 aborted，
// 而被测代码正是要自己开一个事务并回滚它。因此改用 testEntClient（真实 client），
// 靠 FK 违例（绑定一个不存在的 group_id）注入故障，并在末尾清理夹具。

func TestCreateRollsBackMainRowWhenBindingWriteFailsOnPostgres(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newAPIKeyRepositoryWithSQL(client, integrationDB)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	user, err := client.User.Create().
		SetEmail("bindings-rollback-create-" + suffix + "@test.com").
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.User.DeleteOneID(user.ID).Exec(ctx) })

	keyValue := "sk-bindings-rollback-create-" + suffix
	key := &service.APIKey{
		UserID: user.ID,
		Key:    keyValue,
		Name:   "rollback-create",
		Status: service.StatusActive,
		// 不存在的分组 ID：关联表插入会撞外键，整个事务必须回滚。
		BoundGroups: []*service.Group{{ID: 9223372036854775000, Platform: service.PlatformAnthropic}},
	}
	err = repo.Create(ctx, key)
	require.Error(t, err, "关联表 FK 违例必须让 Create 失败")
	require.Contains(t, err.Error(), "api_key_groups_group_id_fkey",
		"失败必须发生在关联表写入这一步，否则用例是空转的")

	_, err = repo.GetByKey(ctx, keyValue)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound, "主表不得留下中间态记录")
}

func TestUpdateRollsBackMainRowWhenBindingWriteFailsOnPostgres(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newAPIKeyRepositoryWithSQL(client, integrationDB)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	user, err := client.User.Create().
		SetEmail("bindings-rollback-update-" + suffix + "@test.com").
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.User.DeleteOneID(user.ID).Exec(ctx) })

	grp, err := client.Group.Create().
		SetName("bindings-rollback-update-" + suffix).
		SetPlatform(service.PlatformAnthropic).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Group.DeleteOneID(grp.ID).Exec(ctx) })

	key := &service.APIKey{
		UserID:      user.ID,
		Key:         "sk-bindings-rollback-update-" + suffix,
		Name:        "before-rollback",
		Status:      service.StatusActive,
		GroupID:     &grp.ID,
		BoundGroups: []*service.Group{groupEntityToService(grp)},
	}
	require.NoError(t, repo.Create(ctx, key))
	t.Cleanup(func() { _ = repo.Delete(ctx, key.ID) })

	key.Name = "after-rollback"
	key.BoundGroups = []*service.Group{{ID: 9223372036854775000, Platform: service.PlatformAnthropic}}
	err = repo.Update(ctx, key, service.APIKeyUpdateFields{Name: true, BoundGroups: true})
	require.Error(t, err, "关联表 FK 违例必须让 Update 失败")
	require.Contains(t, err.Error(), "api_key_groups_group_id_fkey",
		"失败必须发生在关联表写入这一步，否则用例是空转的")

	got, err := repo.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, "before-rollback", got.Name, "主表列的改动必须随关联表失败一起回滚")

	bound, err := repo.ListBoundGroupIDs(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{grp.ID}, bound, "原有绑定不得被半途删除")
}
