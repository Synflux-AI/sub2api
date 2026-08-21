//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// issue #171：API Key 可以绑定多个已有分组（每平台至多一个），按命中分组独立计费。
// 本文件锁死 service 层的三件事：
//  1. 绑定集合的业务校验（同平台唯一、composite 独占、存在性/软删/可见性）；
//  2. 默认组解析规则（platform 字典序 → group id 升序）只有一份实现；
//  3. Create / Update 把主表与关联表**作为一次仓库调用**交出去，掩码两个都置位，
//     从而不会留下「默认组改了、关联表残留旧分组」的同平台双绑。

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

// bindingGroupRepoStub 只实现 GetByIDLite，其余方法沿用 groupRepoStub 的 panic 行为。
//
// **刻意不实现 GetByID**：基类的 GetByID 会 panic("unexpected GetByID call")，
// 于是「绑定解析路径必须走轻量查询」这条要求被钉死在类型上 —— GetByID 会额外跑一次
// loadAccountCounts 聚合，在自助端点上按 group_ids 的长度线性放大查询量。
//
// softDeleted 里的分组返回 ErrGroupNotFound —— 这与真实仓库一致：
// groups 走 ent 软删拦截器，GetByIDLite 查不到已软删的行，服务层无法区分
// 「从未存在」与「已软删」，两者都是 ErrGroupNotFound。
type bindingGroupRepoStub struct {
	groupRepoStub
	groups       map[int64]*Group
	softDeleted  map[int64]bool
	groupLookups []int64
}

func (s *bindingGroupRepoStub) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	s.groupLookups = append(s.groupLookups, id)
	if s.softDeleted[id] {
		return nil, ErrGroupNotFound
	}
	g, ok := s.groups[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	clone := *g
	return &clone, nil
}

// bindingUserSubRepoStub 让订阅类型分组的可绑定判定可控。
type bindingUserSubRepoStub struct {
	userSubRepoNoop
	activeByGroupID map[int64]bool
}

func (s *bindingUserSubRepoStub) GetActiveByUserIDAndGroupID(_ context.Context, _, groupID int64) (*UserSubscription, error) {
	if s.activeByGroupID[groupID] {
		return &UserSubscription{ID: 1, GroupID: groupID}, nil
	}
	return nil, ErrSubscriptionNotFound
}

// bindingAPIKeyRepoStub 记录 Create / Update 收到的对象与掩码。
//
// ReplaceBindings 是**哨兵**：服务层绝不能自己单独调它。主表 + 关联表必须在
// 一次 Create/Update 调用里由仓库层放进同一个事务（C11），
// 服务层拆成两步就等于放弃了原子性。
type bindingAPIKeyRepoStub struct {
	quotaBaseAPIKeyRepoStub

	existing *APIKey

	createErr error
	updateErr error

	created             []*APIKey
	updated             []*APIKey
	updateFields        []APIKeyUpdateFields
	replaceBindingCalls int
}

func (s *bindingAPIKeyRepoStub) GetByID(context.Context, int64) (*APIKey, error) {
	if s.existing == nil {
		return nil, ErrAPIKeyNotFound
	}
	clone := *s.existing
	clone.BoundGroups = append([]*Group(nil), s.existing.BoundGroups...)
	return &clone, nil
}

func (s *bindingAPIKeyRepoStub) Create(_ context.Context, key *APIKey) error {
	snapshot := *key
	snapshot.BoundGroups = append([]*Group(nil), key.BoundGroups...)
	s.created = append(s.created, &snapshot)
	if s.createErr != nil {
		return s.createErr
	}
	key.ID = 101
	return nil
}

func (s *bindingAPIKeyRepoStub) Update(_ context.Context, key *APIKey, fields APIKeyUpdateFields) error {
	snapshot := *key
	snapshot.BoundGroups = append([]*Group(nil), key.BoundGroups...)
	s.updated = append(s.updated, &snapshot)
	s.updateFields = append(s.updateFields, fields)
	return s.updateErr
}

func (s *bindingAPIKeyRepoStub) ReplaceBindings(context.Context, int64, []GroupBinding) error {
	s.replaceBindingCalls++
	return errors.New("service layer must not write api_key_groups on its own")
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	testGroupAnthropicA = int64(10)
	testGroupAnthropicB = int64(20)
	testGroupOpenAI     = int64(30)
	testGroupGemini     = int64(40)
	testGroupComposite  = int64(50)
	testGroupExclusive  = int64(60)
	testGroupSubscribed = int64(70)
	testGroupSoftDelete = int64(80)
	testGroupMissing    = int64(999)
)

func testBindingGroups() map[int64]*Group {
	return map[int64]*Group{
		testGroupAnthropicA: {ID: testGroupAnthropicA, Name: "claude-ccmax", Platform: PlatformAnthropic},
		testGroupAnthropicB: {ID: testGroupAnthropicB, Name: "claude-backup", Platform: PlatformAnthropic},
		testGroupOpenAI:     {ID: testGroupOpenAI, Name: "codex", Platform: PlatformOpenAI},
		testGroupGemini:     {ID: testGroupGemini, Name: "gemini-pro", Platform: PlatformGemini},
		testGroupComposite:  {ID: testGroupComposite, Name: "combo", Platform: PlatformComposite},
		testGroupExclusive:  {ID: testGroupExclusive, Name: "vip", Platform: PlatformGrok, IsExclusive: true},
		testGroupSubscribed: {ID: testGroupSubscribed, Name: "sub-plan", Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription},
		testGroupSoftDelete: {ID: testGroupSoftDelete, Name: "gone", Platform: PlatformOpenAI},
	}
}

type bindingServiceHarness struct {
	svc       *APIKeyService
	apiKeys   *bindingAPIKeyRepoStub
	groups    *bindingGroupRepoStub
	users     *userRepoStub
	userSubs  *bindingUserSubRepoStub
	authCache *quotaStateCacheStub
}

func newBindingServiceHarness(t *testing.T, user *User, existing *APIKey) *bindingServiceHarness {
	t.Helper()
	h := &bindingServiceHarness{
		apiKeys:   &bindingAPIKeyRepoStub{existing: existing},
		groups:    &bindingGroupRepoStub{groups: testBindingGroups(), softDeleted: map[int64]bool{testGroupSoftDelete: true}},
		users:     &userRepoStub{usersByID: map[int64]*User{user.ID: user}},
		userSubs:  &bindingUserSubRepoStub{activeByGroupID: map[int64]bool{}},
		authCache: &quotaStateCacheStub{},
	}
	h.svc = &APIKeyService{
		apiKeyRepo:  h.apiKeys,
		userRepo:    h.users,
		groupRepo:   h.groups,
		userSubRepo: h.userSubs,
		cache:       h.authCache,
		cfg:         &config.Config{},
	}
	return h
}

func testBindingUser() *User {
	return &User{ID: 7, Status: StatusActive}
}

// groupIDsPtr 构造 *[]int64 —— Create / Update 的 GroupIDs 都是三态指针：
// nil = 请求没带该字段；非 nil 空切片 = 显式清空；非空 = 整体替换。
// 不传参数时返回**非 nil 的空切片指针**（显式清空），与 nil 语义不同。
func groupIDsPtr(ids ...int64) *[]int64 {
	out := make([]int64, 0, len(ids))
	out = append(out, ids...)
	return &out
}

func boundGroupIDs(groups []*Group) []int64 {
	out := make([]int64, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// 纯函数：ValidateGroupBindingSet
// ---------------------------------------------------------------------------

func TestValidateGroupBindingSet_EmptySetIsAllowed(t *testing.T) {
	// 空绑定集合 = 未分组 Key，维持现有语义（C5），不得在这里提前报错。
	require.NoError(t, ValidateGroupBindingSet(nil))
	require.NoError(t, ValidateGroupBindingSet([]*Group{}))
}

func TestValidateGroupBindingSet_OneGroupPerPlatformIsAllowed(t *testing.T) {
	groups := testBindingGroups()
	require.NoError(t, ValidateGroupBindingSet([]*Group{
		groups[testGroupAnthropicA], groups[testGroupOpenAI], groups[testGroupGemini],
	}))
}

func TestValidateGroupBindingSet_RejectsSamePlatformTwiceAndNamesBothGroups(t *testing.T) {
	groups := testBindingGroups()
	err := ValidateGroupBindingSet([]*Group{
		groups[testGroupAnthropicA], groups[testGroupOpenAI], groups[testGroupAnthropicB],
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAPIKeyGroupPlatformConflict)

	// 错误必须点名冲突的平台与**两个**分组，用户才知道该去掉哪一个。
	msg := err.Error()
	require.Contains(t, msg, PlatformAnthropic)
	require.Contains(t, msg, "claude-ccmax")
	require.Contains(t, msg, "claude-backup")
	// 不相关的分组不该出现在文案里。
	require.NotContains(t, msg, "codex")
}

func TestValidateGroupBindingSet_SameGroupTwiceIsDeduped(t *testing.T) {
	// 同一个分组给两次不是「同平台冲突」，按去重处理。
	groups := testBindingGroups()
	require.NoError(t, ValidateGroupBindingSet([]*Group{
		groups[testGroupAnthropicA], groups[testGroupAnthropicA],
	}))
}

func TestValidateGroupBindingSet_RejectsCompositeMixedWithNormalGroup(t *testing.T) {
	groups := testBindingGroups()

	// composite 在前
	err := ValidateGroupBindingSet([]*Group{groups[testGroupComposite], groups[testGroupOpenAI]})
	require.ErrorIs(t, err, ErrAPIKeyCompositeGroupExclusive)
	require.Contains(t, err.Error(), "combo")
	require.Contains(t, err.Error(), "codex")

	// 普通组在前（反之亦然，同样拒绝）
	err = ValidateGroupBindingSet([]*Group{groups[testGroupOpenAI], groups[testGroupComposite]})
	require.ErrorIs(t, err, ErrAPIKeyCompositeGroupExclusive)
}

func TestValidateGroupBindingSet_CompositeAloneIsAllowed(t *testing.T) {
	groups := testBindingGroups()
	require.NoError(t, ValidateGroupBindingSet([]*Group{groups[testGroupComposite]}))
}

func TestValidateGroupBindingSet_TwoCompositeGroupsAreSamePlatformConflict(t *testing.T) {
	// 两个 composite 组的 platform 相同，先被同平台唯一这条规则拦下。
	err := ValidateGroupBindingSet([]*Group{
		{ID: 1, Name: "combo-a", Platform: PlatformComposite},
		{ID: 2, Name: "combo-b", Platform: PlatformComposite},
	})
	require.ErrorIs(t, err, ErrAPIKeyGroupPlatformConflict)
}

// ---------------------------------------------------------------------------
// 纯函数：ResolveDefaultGroupID
// ---------------------------------------------------------------------------

func TestResolveDefaultGroupID_EmptySetHasNoDefault(t *testing.T) {
	require.Nil(t, ResolveDefaultGroupID(nil))
	require.Nil(t, ResolveDefaultGroupID([]GroupBinding{}))
	require.Nil(t, ResolveDefaultGroupIDFromGroups(nil))
}

func TestResolveDefaultGroupID_PlatformOrderBeatsGroupID(t *testing.T) {
	// anthropic < openai（字典序），即使 anthropic 的 id 更大也要选它。
	got := ResolveDefaultGroupID([]GroupBinding{
		{GroupID: 1, Platform: PlatformOpenAI},
		{GroupID: 999, Platform: PlatformAnthropic},
	})
	require.NotNil(t, got)
	require.Equal(t, int64(999), *got)
}

func TestResolveDefaultGroupID_SamePlatformPicksLowestID(t *testing.T) {
	got := ResolveDefaultGroupID([]GroupBinding{
		{GroupID: 77, Platform: PlatformAnthropic},
		{GroupID: 12, Platform: PlatformAnthropic},
		{GroupID: 45, Platform: PlatformAnthropic},
	})
	require.NotNil(t, got)
	require.Equal(t, int64(12), *got)
}

func TestResolveDefaultGroupID_IsIndependentOfInputOrder(t *testing.T) {
	// 同一份集合的不同排列必须解析出同一个默认组 —— T7 删组后改选默认组
	// 与 T3 创建时解析默认组共用这个函数，结果必须可复现。
	permutations := [][]GroupBinding{
		{{GroupID: 40, Platform: PlatformGemini}, {GroupID: 30, Platform: PlatformOpenAI}, {GroupID: 20, Platform: PlatformAnthropic}, {GroupID: 10, Platform: PlatformAnthropic}},
		{{GroupID: 10, Platform: PlatformAnthropic}, {GroupID: 20, Platform: PlatformAnthropic}, {GroupID: 30, Platform: PlatformOpenAI}, {GroupID: 40, Platform: PlatformGemini}},
		{{GroupID: 30, Platform: PlatformOpenAI}, {GroupID: 10, Platform: PlatformAnthropic}, {GroupID: 40, Platform: PlatformGemini}, {GroupID: 20, Platform: PlatformAnthropic}},
	}
	for i, bindings := range permutations {
		got := ResolveDefaultGroupID(bindings)
		require.NotNil(t, got, "permutation %d", i)
		require.Equal(t, int64(10), *got, "permutation %d", i)
	}
}

func TestResolveDefaultGroupID_DoesNotMutateInput(t *testing.T) {
	bindings := []GroupBinding{
		{GroupID: 30, Platform: PlatformOpenAI},
		{GroupID: 10, Platform: PlatformAnthropic},
	}
	_ = ResolveDefaultGroupID(bindings)
	require.Equal(t, int64(30), bindings[0].GroupID, "ResolveDefaultGroupID 不得就地排序调用方的切片")
	require.Equal(t, int64(10), bindings[1].GroupID)
}

func TestResolveDefaultGroupID_SingleBinding(t *testing.T) {
	got := ResolveDefaultGroupID([]GroupBinding{{GroupID: 5, Platform: PlatformGrok}})
	require.NotNil(t, got)
	require.Equal(t, int64(5), *got)
}

func TestResolveDefaultGroupID_MatchesFirstElementOfSortedBoundGroups(t *testing.T) {
	// 把「BoundGroups 的排序约定」与「默认组选取规则」钉在一起：
	// 默认组必须恰好是 SortBoundGroups 之后的第一个元素。两者共用 lessGroupBinding。
	groups := testBindingGroups()
	set := []*Group{groups[testGroupGemini], groups[testGroupOpenAI], groups[testGroupAnthropicB], groups[testGroupAnthropicA]}
	SortBoundGroups(set)

	got := ResolveDefaultGroupIDFromGroups(set)
	require.NotNil(t, got)
	require.Equal(t, set[0].ID, *got)
	require.Equal(t, testGroupAnthropicA, *got)
}

// ---------------------------------------------------------------------------
// 纯函数：新旧入参归一化
// ---------------------------------------------------------------------------

// 完整的六格语义矩阵。每一行都是**上游裁定**的行为，不得擅自改动：
//
//	| group_ids     | group_id | 现有绑定数 | 行为                                              |
//	| nil（缺省）   | X        | <= 1       | 整体替换成 [X]（改造前的单分组语义，逐字不变）      |
//	| nil（缺省）   | X        | >= 2       | 只改默认组：集合不动，默认组 = X；X 必须 ∈ 现有集合 |
//	| nil（缺省）   | nil      | 任意       | 空集合（Create = 未分组；Update 根本不进这个分支）  |
//	| 非 nil 空切片 | 任意     | 任意       | 清空全部绑定（显式解绑优先，**不回退旧字段**）      |
//	| 非空          | nil      | 任意       | 集合 = group_ids，默认组按稳定规则解析              |
//	| 非空          | X        | 任意       | 集合 = group_ids，默认组 = X；X 必须 ∈ 集合         |
func TestResolveAPIKeyGroupBindingIntent_CoversFullSemanticMatrix(t *testing.T) {
	legacy := int64(10)
	other := int64(30)

	tests := []struct {
		name           string
		groupIDs       *[]int64
		legacyGroupID  *int64
		existingCount  int
		wantMode       apiKeyGroupBindingMode
		wantGroupIDs   []int64
		wantDefaultNil bool
		wantDefault    int64
	}{
		{
			name: "① 缺省 group_ids + group_id + 现有绑定 0 个 → 整体替换成单组",
			// 未分组 Key 收到 group_id=X：与改造前逐字相同（C3）。
			groupIDs: nil, legacyGroupID: &legacy, existingCount: 0,
			wantMode: apiKeyBindingModeReplaceSet, wantGroupIDs: []int64{10}, wantDefault: 10,
		},
		{
			name: "① 缺省 group_ids + group_id + 现有绑定 1 个 → 整体替换成单组",
			// 单分组 Key 换组：改造前就是「丢掉旧组、绑上新组」，必须逐字不变（C3）。
			groupIDs: nil, legacyGroupID: &legacy, existingCount: 1,
			wantMode: apiKeyBindingModeReplaceSet, wantGroupIDs: []int64{10}, wantDefault: 10,
		},
		{
			name: "② 缺省 group_ids + group_id + 现有绑定 2 个 → 只改默认组",
			// 多分组 Key 遇到只会发旧单值字段的客户端：不能把集合缩成一个组，
			// 否则其它平台的绑定被静默丢弃。
			groupIDs: nil, legacyGroupID: &legacy, existingCount: 2,
			wantMode: apiKeyBindingModeDefaultOnly, wantGroupIDs: nil, wantDefault: 10,
		},
		{
			name:     "② 缺省 group_ids + group_id + 现有绑定 5 个 → 只改默认组",
			groupIDs: nil, legacyGroupID: &legacy, existingCount: 5,
			wantMode: apiKeyBindingModeDefaultOnly, wantGroupIDs: nil, wantDefault: 10,
		},
		{
			name:     "③ 两个都缺省 → 空集合，没有默认组",
			groupIDs: nil, legacyGroupID: nil, existingCount: 3,
			wantMode: apiKeyBindingModeReplaceSet, wantGroupIDs: nil, wantDefaultNil: true,
		},
		{
			name: "④ 非 nil 空切片 + group_id → 清空，不回退旧字段",
			// 这一格是 review Important 1 的核心：改造前的归一化在 len(group_ids)==0 时
			// **无条件**回退旧 group_id，于是 group_ids=[] + group_id=X 会绑上 X 而不是清空，
			// 用户点「移除所有分组」被静默改回旧默认组。
			groupIDs: groupIDsPtr(), legacyGroupID: &legacy, existingCount: 2,
			wantMode: apiKeyBindingModeReplaceSet, wantGroupIDs: nil, wantDefaultNil: true,
		},
		{
			name:     "④ 非 nil 空切片 + 无 group_id → 清空",
			groupIDs: groupIDsPtr(), legacyGroupID: nil, existingCount: 2,
			wantMode: apiKeyBindingModeReplaceSet, wantGroupIDs: nil, wantDefaultNil: true,
		},
		{
			name:     "⑤ 非空 group_ids + 无 group_id → 集合替换，默认组交给稳定规则",
			groupIDs: groupIDsPtr(30, 10), legacyGroupID: nil, existingCount: 4,
			wantMode: apiKeyBindingModeReplaceSet, wantGroupIDs: []int64{30, 10}, wantDefaultNil: true,
		},
		{
			name:     "⑥ 非空 group_ids + group_id → 集合替换，默认组显式指定",
			groupIDs: groupIDsPtr(30, 10), legacyGroupID: &other, existingCount: 4,
			wantMode: apiKeyBindingModeReplaceSet, wantGroupIDs: []int64{30, 10}, wantDefault: 30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAPIKeyGroupBindingIntent(tt.groupIDs, tt.legacyGroupID, tt.existingCount)
			require.Equal(t, tt.wantMode, got.Mode)
			require.Equal(t, tt.wantGroupIDs, got.GroupIDs)
			if tt.wantDefaultNil {
				require.Nil(t, got.DefaultGroupID)
				return
			}
			require.NotNil(t, got.DefaultGroupID)
			require.Equal(t, tt.wantDefault, *got.DefaultGroupID)
		})
	}
}

// DefaultOnly 模式下 DefaultGroupID 必然非 nil —— resolveAPIKeyGroupBindingPlan 直接解引用它。
// 这条不变量替代了一个从唯一入口不可达的防御分支（review Minor 3）：
// 与其留一段死代码（而且它当年表达的意图与正确行为相反），不如把不变量钉在测试里。
func TestResolveAPIKeyGroupBindingIntent_DefaultOnlyAlwaysCarriesDefaultGroupID(t *testing.T) {
	legacy := int64(10)
	candidates := []*[]int64{nil, groupIDsPtr(), groupIDsPtr(1), groupIDsPtr(1, 2)}
	for _, ids := range candidates {
		for _, legacyID := range []*int64{nil, &legacy} {
			for existing := 0; existing <= 4; existing++ {
				got := resolveAPIKeyGroupBindingIntent(ids, legacyID, existing)
				if got.Mode != apiKeyBindingModeDefaultOnly {
					continue
				}
				require.NotNil(t, got.DefaultGroupID,
					"DefaultOnly 模式必须带上默认组（ids=%v legacy=%v existing=%d）", ids, legacyID, existing)
				require.Empty(t, got.GroupIDs, "DefaultOnly 模式不得携带待替换的集合")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Create：校验矩阵
// ---------------------------------------------------------------------------

func TestAPIKeyCreate_GroupBindingValidationMatrix(t *testing.T) {
	tests := []struct {
		name        string
		user        *User
		req         CreateAPIKeyRequest
		wantErrIs   error
		wantErrMsgs []string
	}{
		{
			name:      "同平台重复绑定",
			user:      testBindingUser(),
			req:       CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupAnthropicB)},
			wantErrIs: ErrAPIKeyGroupPlatformConflict,
			// 文案必须点名平台与两个分组。
			wantErrMsgs: []string{PlatformAnthropic, "claude-ccmax", "claude-backup"},
		},
		{
			name:        "composite 与普通组混绑",
			user:        testBindingUser(),
			req:         CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupComposite, testGroupOpenAI)},
			wantErrIs:   ErrAPIKeyCompositeGroupExclusive,
			wantErrMsgs: []string{"combo", "codex"},
		},
		{
			name:      "分组不存在",
			user:      testBindingUser(),
			req:       CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupMissing)},
			wantErrIs: ErrGroupNotFound,
		},
		{
			name: "分组已软删（与不存在同一错误）",
			user: testBindingUser(),
			req:  CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupSoftDelete)},
			// groups 走 ent 软删拦截器，GetByID 查不到已软删行 → 与「从未存在」同一错误。
			wantErrIs: ErrGroupNotFound,
		},
		{
			name:      "专属分组不在用户允许名单",
			user:      testBindingUser(),
			req:       CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupExclusive)},
			wantErrIs: ErrGroupNotAllowed,
		},
		{
			name:      "订阅类型分组没有有效订阅",
			user:      testBindingUser(),
			req:       CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupSubscribed)},
			wantErrIs: ErrGroupNotAllowed,
		},
		{
			name:      "显式默认组不在绑定集合内",
			user:      testBindingUser(),
			req:       CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupOpenAI), GroupID: ptrInt64(testGroupGemini)},
			wantErrIs: ErrAPIKeyDefaultGroupNotBound,
		},
		{
			name:      "多个组里有一个无权限",
			user:      testBindingUser(),
			req:       CreateAPIKeyRequest{Name: "k", GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupExclusive)},
			wantErrIs: ErrGroupNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBindingServiceHarness(t, tt.user, nil)

			key, err := h.svc.Create(context.Background(), tt.user.ID, tt.req)
			require.Error(t, err)
			require.Nil(t, key)
			require.ErrorIs(t, err, tt.wantErrIs)
			for _, want := range tt.wantErrMsgs {
				require.Contains(t, err.Error(), want)
			}

			// 校验失败必须发生在任何写入之前 —— 没有中间态可言。
			require.Empty(t, h.apiKeys.created, "校验失败后不得写主表")
			require.Zero(t, h.apiKeys.replaceBindingCalls, "校验失败后不得写关联表")
			require.Empty(t, h.authCache.deleteAuthKeys, "校验失败后不得失效认证缓存")
		})
	}
}

func TestAPIKeyCreate_AllowsExclusiveGroupOnAllowList(t *testing.T) {
	user := testBindingUser()
	user.AllowedGroups = []int64{testGroupExclusive}
	h := newBindingServiceHarness(t, user, nil)

	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: groupIDsPtr(testGroupExclusive),
	})
	require.NoError(t, err)
	require.Equal(t, []int64{testGroupExclusive}, boundGroupIDs(key.BoundGroups))
}

func TestAPIKeyCreate_AllowsSubscriptionGroupWithActiveSubscription(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)
	h.userSubs.activeByGroupID[testGroupSubscribed] = true

	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: groupIDsPtr(testGroupSubscribed),
	})
	require.NoError(t, err)
	require.Equal(t, []int64{testGroupSubscribed}, boundGroupIDs(key.BoundGroups))
}

// ---------------------------------------------------------------------------
// Create：默认组解析与新旧接口兼容
// ---------------------------------------------------------------------------

func TestAPIKeyCreate_EmptyBindingSetKeepsUngroupedSemantics(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{Name: "k"})
	require.NoError(t, err)
	require.Nil(t, key.GroupID)
	require.Nil(t, key.Group)
	require.Empty(t, key.BoundGroups)

	require.Len(t, h.apiKeys.created, 1)
	require.Nil(t, h.apiKeys.created[0].GroupID)
	require.Empty(t, h.apiKeys.created[0].BoundGroups)
	require.Empty(t, h.groups.groupLookups, "空集合不该去查任何分组")
}

func TestAPIKeyCreate_LegacyGroupIDBindsThatGroupAndAlsoWritesAssociation(t *testing.T) {
	// 旧接口只带 group_id → 等价于 group_ids = [group_id]。
	// 关键：BoundGroups 必须一起交给仓库层，否则默认组不会出现在 api_key_groups 里，
	// 迁移 230 声明的「group_id 指向的分组必然在关联表」就不成立（spec §1）。
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupID: ptrInt64(testGroupOpenAI),
	})
	require.NoError(t, err)
	require.NotNil(t, key.GroupID)
	require.Equal(t, testGroupOpenAI, *key.GroupID)
	require.NotNil(t, key.Group)
	require.Equal(t, testGroupOpenAI, key.Group.ID)

	require.Len(t, h.apiKeys.created, 1)
	created := h.apiKeys.created[0]
	require.Equal(t, []int64{testGroupOpenAI}, boundGroupIDs(created.BoundGroups))
	require.NotNil(t, created.GroupID)
	require.Equal(t, testGroupOpenAI, *created.GroupID)
	// 默认组必然是绑定集合的成员。
	require.Contains(t, boundGroupIDs(created.BoundGroups), *created.GroupID)
}

func TestAPIKeyCreate_GroupIDsOnlyResolvesDefaultByStableRule(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	// 故意乱序传入：gemini(40) / openai(30) / anthropic(20)。
	// 规则 = platform 字典序 → anthropic 最小 → 默认组是 20。
	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: groupIDsPtr(testGroupGemini, testGroupOpenAI, testGroupAnthropicB),
	})
	require.NoError(t, err)
	require.NotNil(t, key.GroupID)
	require.Equal(t, testGroupAnthropicB, *key.GroupID)
	require.NotNil(t, key.Group)
	require.Equal(t, testGroupAnthropicB, key.Group.ID)

	// BoundGroups 按 (platform, id) 稳定排序：anthropic → gemini → openai。
	require.Equal(t,
		[]int64{testGroupAnthropicB, testGroupGemini, testGroupOpenAI},
		boundGroupIDs(h.apiKeys.created[0].BoundGroups),
	)
}

func TestAPIKeyCreate_ExplicitDefaultGroupInsideSetWins(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	// 稳定规则会选 anthropic(10)，但调用方显式指定了 openai(30)。
	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name:     "k",
		GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupOpenAI),
		GroupID:  ptrInt64(testGroupOpenAI),
	})
	require.NoError(t, err)
	require.NotNil(t, key.GroupID)
	require.Equal(t, testGroupOpenAI, *key.GroupID)
	require.Contains(t, boundGroupIDs(key.BoundGroups), testGroupOpenAI)
}

func TestAPIKeyCreate_DuplicateGroupIDsAreDedupedAndQueriedOnce(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupAnthropicA, testGroupOpenAI),
	})
	require.NoError(t, err)
	require.Equal(t, []int64{testGroupAnthropicA, testGroupOpenAI}, boundGroupIDs(key.BoundGroups))
	require.Equal(t, []int64{testGroupAnthropicA, testGroupOpenAI}, h.groups.groupLookups)
}

// ---------------------------------------------------------------------------
// Update：本 Task 的硬性验收项
// ---------------------------------------------------------------------------

// 改默认组 A→B（**同平台**）后，绑定集合里不得残留 A。
//
// 这是 issue #171 T2 review 实测出的读模型级不变量破裂：掩码未接线时只置
// fields.GroupID，关联表残留 A，BoundGroups 读出来是同平台双绑 {A, B}。
func TestAPIKeyUpdate_LegacyGroupIDSwitchOnSamePlatformLeavesNoDoubleBinding(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		Group:       groups[testGroupAnthropicA],
		BoundGroups: []*Group{groups[testGroupAnthropicA]},
	}
	h := newBindingServiceHarness(t, user, existing)

	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{
		GroupID: ptrInt64(testGroupAnthropicB),
	})
	require.NoError(t, err)

	require.Len(t, h.apiKeys.updated, 1)
	written := h.apiKeys.updated[0]

	// ① 关联表被整体替换成只含 B —— A 必须消失。
	require.Equal(t, []int64{testGroupAnthropicB}, boundGroupIDs(written.BoundGroups))
	require.NotContains(t, boundGroupIDs(written.BoundGroups), testGroupAnthropicA)

	// ② 绑定集合本身仍满足同平台唯一这条不变量（不是「B 在集合里」这么弱的断言）。
	require.NoError(t, ValidateGroupBindingSet(written.BoundGroups))

	// ③ 两个掩码同时置位，仓库层才会在同一事务里既改 group_id 又重写关联表。
	require.Len(t, h.apiKeys.updateFields, 1)
	require.True(t, h.apiKeys.updateFields[0].GroupID, "必须置位 fields.GroupID")
	require.True(t, h.apiKeys.updateFields[0].BoundGroups,
		"必须置位 fields.BoundGroups，否则关联表残留旧的同平台分组")

	// ④ 返回值自身一致：group_id / group / bound_groups 三者不得自相矛盾。
	require.NotNil(t, key.GroupID)
	require.Equal(t, testGroupAnthropicB, *key.GroupID)
	require.NotNil(t, key.Group)
	require.Equal(t, testGroupAnthropicB, key.Group.ID, "默认组换了，返回的 Group 不能还是旧分组")
	require.Equal(t, []int64{testGroupAnthropicB}, boundGroupIDs(key.BoundGroups))
}

func TestAPIKeyUpdate_WithoutGroupInputLeavesBindingsUntouched(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		BoundGroups: []*Group{groups[testGroupAnthropicA], groups[testGroupOpenAI]},
	}
	h := newBindingServiceHarness(t, user, existing)

	name := "renamed"
	_, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{Name: &name})
	require.NoError(t, err)

	require.Equal(t, []APIKeyUpdateFields{{Name: true}}, h.apiKeys.updateFields,
		"不改分组的请求不得置位 GroupID / BoundGroups")
	require.Empty(t, h.groups.groupLookups, "不改分组时不该查分组")
}

func TestAPIKeyUpdate_EmptyGroupIDsClearsAllBindings(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		Group:       groups[testGroupAnthropicA],
		BoundGroups: []*Group{groups[testGroupAnthropicA], groups[testGroupOpenAI]},
	}
	h := newBindingServiceHarness(t, user, existing)

	empty := []int64{}
	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &empty})
	require.NoError(t, err)

	require.Len(t, h.apiKeys.updated, 1)
	require.Nil(t, h.apiKeys.updated[0].GroupID)
	require.Empty(t, h.apiKeys.updated[0].BoundGroups)
	require.True(t, h.apiKeys.updateFields[0].GroupID)
	require.True(t, h.apiKeys.updateFields[0].BoundGroups)

	require.Nil(t, key.GroupID)
	require.Nil(t, key.Group)
	require.Empty(t, key.BoundGroups)
}

func TestAPIKeyUpdate_ReplacesBindingSetAndResolvesDefault(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupOpenAI),
		Group:       groups[testGroupOpenAI],
		BoundGroups: []*Group{groups[testGroupOpenAI]},
	}
	h := newBindingServiceHarness(t, user, existing)

	ids := []int64{testGroupGemini, testGroupAnthropicA}
	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &ids})
	require.NoError(t, err)

	// 旧的 openai 绑定被整体替换掉。
	require.Equal(t, []int64{testGroupAnthropicA, testGroupGemini}, boundGroupIDs(key.BoundGroups))
	require.NotNil(t, key.GroupID)
	require.Equal(t, testGroupAnthropicA, *key.GroupID)
	require.NoError(t, ValidateGroupBindingSet(h.apiKeys.updated[0].BoundGroups))
}

func TestAPIKeyUpdate_RejectsExplicitDefaultOutsideGroupIDs(t *testing.T) {
	user := testBindingUser()
	existing := &APIKey{ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive}
	h := newBindingServiceHarness(t, user, existing)

	ids := []int64{testGroupAnthropicA}
	_, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{
		GroupIDs: &ids, GroupID: ptrInt64(testGroupOpenAI),
	})
	require.ErrorIs(t, err, ErrAPIKeyDefaultGroupNotBound)
	require.Empty(t, h.apiKeys.updated, "校验失败不得写主表")
	require.Zero(t, h.apiKeys.replaceBindingCalls)
	require.Empty(t, h.authCache.deleteAuthKeys)
}

func TestAPIKeyUpdate_RejectsSamePlatformConflictInNewSet(t *testing.T) {
	user := testBindingUser()
	existing := &APIKey{ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive}
	h := newBindingServiceHarness(t, user, existing)

	ids := []int64{testGroupAnthropicA, testGroupAnthropicB}
	_, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &ids})
	require.ErrorIs(t, err, ErrAPIKeyGroupPlatformConflict)
	require.Empty(t, h.apiKeys.updated)
}

func TestAPIKeyUpdate_RejectsCompositeMixedWithNormalGroup(t *testing.T) {
	user := testBindingUser()
	existing := &APIKey{ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive}
	h := newBindingServiceHarness(t, user, existing)

	ids := []int64{testGroupComposite, testGroupGemini}
	_, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &ids})
	require.ErrorIs(t, err, ErrAPIKeyCompositeGroupExclusive)
	require.Empty(t, h.apiKeys.updated)
}

// ---------------------------------------------------------------------------
// 事务边界：主表 + 关联表必须是一次仓库调用；失败后没有中间态
// ---------------------------------------------------------------------------

// 服务层不得把「写主表」和「写关联表」拆成两次仓库调用 —— 那样就没有共同的事务边界了。
// ReplaceBindings 在 stub 里是哨兵，一旦被服务层直接调用就会被这条断言抓住（C11）。
func TestAPIKeyCreate_HandsMainRowAndBindingsToRepositoryInOneCall(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	_, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupOpenAI),
	})
	require.NoError(t, err)
	require.Len(t, h.apiKeys.created, 1, "只能有一次仓库写调用")
	require.Zero(t, h.apiKeys.replaceBindingCalls,
		"服务层不得自己调 ReplaceBindings：主表与关联表必须由仓库层放进同一个事务")
	require.NotEmpty(t, h.apiKeys.created[0].BoundGroups, "绑定集合必须随同一次调用交给仓库层")
}

func TestAPIKeyUpdate_HandsMainRowAndBindingsToRepositoryInOneCall(t *testing.T) {
	user := testBindingUser()
	existing := &APIKey{ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive}
	h := newBindingServiceHarness(t, user, existing)

	ids := []int64{testGroupAnthropicA, testGroupOpenAI}
	_, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &ids})
	require.NoError(t, err)
	require.Len(t, h.apiKeys.updated, 1, "只能有一次仓库写调用")
	require.Zero(t, h.apiKeys.replaceBindingCalls)
	require.NotEmpty(t, h.apiKeys.updated[0].BoundGroups)
}

// 仓库层的事务失败（主表已写、关联表写失败 → 整体回滚）必须一路向上冒泡：
// 服务层不得吞掉错误、不得返回半成品对象、也不得在失败后失效认证缓存
// （缓存失效发生在失败路径上会把「其实没改成」的状态当成改成了）。
func TestAPIKeyCreate_PropagatesBindingWriteFailureWithoutSideEffects(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)
	bindingWriteErr := errors.New("replace bindings: rollback")
	h.apiKeys.createErr = bindingWriteErr

	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupOpenAI),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, bindingWriteErr)
	require.Nil(t, key, "失败时不得返回半成品 APIKey")
	require.Empty(t, h.authCache.deleteAuthKeys, "写入失败后不得失效认证缓存")
}

func TestAPIKeyUpdate_PropagatesBindingWriteFailureWithoutSideEffects(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		BoundGroups: []*Group{groups[testGroupAnthropicA]},
	}
	h := newBindingServiceHarness(t, user, existing)
	bindingWriteErr := errors.New("replace bindings: rollback")
	h.apiKeys.updateErr = bindingWriteErr

	ids := []int64{testGroupAnthropicB, testGroupOpenAI}
	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &ids})
	require.Error(t, err)
	require.ErrorIs(t, err, bindingWriteErr)
	require.Nil(t, key)
	require.Empty(t, h.authCache.deleteAuthKeys)

	// 库里的原始对象没有被服务层就地改写（stub 每次返回克隆，这里确认它没被污染）。
	require.Equal(t, []int64{testGroupAnthropicA}, boundGroupIDs(existing.BoundGroups))
	require.NotNil(t, existing.GroupID)
	require.Equal(t, testGroupAnthropicA, *existing.GroupID)
}

// 成功路径才失效缓存，而且只失效**这一把 Key** 的 entry。
//
// 认证缓存按 key 字符串分桶，本次只有这把 Key 的快照变了。C12 的「按所有绑定分组失效」
// 约束的是组级操作的反查方向（删组 / 改 platform / admin 批量替换，T7），
// 不是单 Key 的自助 CRUD —— 照字面按组反查会把该组下成千上万把无关 Key 的缓存
// 在请求路径内同步清空（写风暴 + 回源雪崩），收益为零。
func TestAPIKeyUpdate_InvalidatesOnlyThisKeysAuthCache(t *testing.T) {
	user := testBindingUser()
	existing := &APIKey{ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive}
	h := newBindingServiceHarness(t, user, existing)

	ids := []int64{testGroupAnthropicA, testGroupOpenAI}
	_, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &ids})
	require.NoError(t, err)

	require.Equal(t, []string{h.svc.authCacheKey("sk-existing")}, h.authCache.deleteAuthKeys)
}

// ---------------------------------------------------------------------------
// 语义矩阵在 Create / Update 上的端到端落地（review Important 1）
// ---------------------------------------------------------------------------

// 矩阵第 1 行（C3）：单分组 Key 只发旧 group_id，**跨平台**换组时整体替换成单组。
// 改造前的语义就是「丢掉旧组、绑上新组」，这里必须逐字不变 ——
// 不能因为多分组改造而变成「往集合里追加一个组」。
func TestAPIKeyUpdate_LegacyGroupIDOnSingleGroupKeyReplacesWholeSet(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		Group:       groups[testGroupAnthropicA],
		BoundGroups: []*Group{groups[testGroupAnthropicA]},
	}
	h := newBindingServiceHarness(t, user, existing)

	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{
		GroupID: ptrInt64(testGroupOpenAI),
	})
	require.NoError(t, err)

	require.Len(t, h.apiKeys.updated, 1)
	require.Equal(t, []int64{testGroupOpenAI}, boundGroupIDs(h.apiKeys.updated[0].BoundGroups),
		"单分组 Key 跨平台换组是**替换**，不是追加")
	require.NotNil(t, key.GroupID)
	require.Equal(t, testGroupOpenAI, *key.GroupID)
}

// 矩阵第 2 行：多分组 Key 只发旧 group_id → 集合保持不动，只换默认组。
//
// 这是 review Important 1 的另一半：改造前的「等价于 group_ids=[group_id]」会让
// 一个只会发单值字段的旧客户端（第三方脚本 / T10 之前的前端）一次编辑就静默删掉
// 其它平台的绑定。
func TestAPIKeyUpdate_LegacyGroupIDOnMultiGroupKeyOnlyChangesDefault(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		Group:       groups[testGroupAnthropicA],
		BoundGroups: []*Group{groups[testGroupAnthropicA], groups[testGroupOpenAI]},
	}
	h := newBindingServiceHarness(t, user, existing)

	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{
		GroupID: ptrInt64(testGroupOpenAI),
	})
	require.NoError(t, err)

	require.Len(t, h.apiKeys.updated, 1)
	written := h.apiKeys.updated[0]
	// ① 绑定集合一个不少 —— openai 的绑定不得被静默丢掉。
	require.Equal(t, []int64{testGroupAnthropicA, testGroupOpenAI}, boundGroupIDs(written.BoundGroups))
	// ② 默认组换成了请求指定的那个。
	require.NotNil(t, written.GroupID)
	require.Equal(t, testGroupOpenAI, *written.GroupID)
	// ③ 返回值三者一致。
	require.NotNil(t, key.GroupID)
	require.Equal(t, testGroupOpenAI, *key.GroupID)
	require.NotNil(t, key.Group)
	require.Equal(t, testGroupOpenAI, key.Group.ID)
	require.Equal(t, []int64{testGroupAnthropicA, testGroupOpenAI}, boundGroupIDs(key.BoundGroups))
	// ④ 这条路径纯内存，不新增任何分组查询。
	require.Empty(t, h.groups.groupLookups, "只改默认组不需要重新解析分组")
}

// 矩阵第 2 行的拒绝分支：默认组必须落在**现有**绑定集合内，否则 400。
// 要把一个新分组变成默认组，必须走 group_ids 把它加进集合。
func TestAPIKeyUpdate_LegacyGroupIDOutsideExistingSetOnMultiGroupKeyIsRejected(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		Group:       groups[testGroupAnthropicA],
		BoundGroups: []*Group{groups[testGroupAnthropicA], groups[testGroupOpenAI]},
	}
	h := newBindingServiceHarness(t, user, existing)

	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{
		GroupID: ptrInt64(testGroupGemini),
	})
	require.ErrorIs(t, err, ErrAPIKeyDefaultGroupNotBound)
	require.Nil(t, key)
	require.Empty(t, h.apiKeys.updated, "拒绝的请求不得写主表")
	require.Zero(t, h.apiKeys.replaceBindingCalls)
	require.Empty(t, h.authCache.deleteAuthKeys)
	// 原始绑定集合没被就地改写。
	require.Equal(t, []int64{testGroupAnthropicA, testGroupOpenAI}, boundGroupIDs(existing.BoundGroups))
}

// 矩阵第 4 行：非 nil 的空切片是**显式解绑**，必须优先于同时带上的旧 group_id。
//
// 改造前的归一化在 len(group_ids)==0 时无条件回退旧字段，于是「清空」被静默改回旧默认组
// —— review Important 1 的原始症状。
func TestAPIKeyUpdate_ExplicitEmptyGroupIDsBeatsLegacyGroupID(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		Group:       groups[testGroupAnthropicA],
		BoundGroups: []*Group{groups[testGroupAnthropicA], groups[testGroupOpenAI]},
	}
	h := newBindingServiceHarness(t, user, existing)

	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{
		GroupIDs: groupIDsPtr(),                 // 显式清空
		GroupID:  ptrInt64(testGroupAnthropicA), // 旧客户端顺带回传的默认组，必须被忽略
	})
	require.NoError(t, err)

	require.Len(t, h.apiKeys.updated, 1)
	require.Nil(t, h.apiKeys.updated[0].GroupID, "显式清空后不得回退到旧 group_id")
	require.Empty(t, h.apiKeys.updated[0].BoundGroups)
	require.True(t, h.apiKeys.updateFields[0].GroupID)
	require.True(t, h.apiKeys.updateFields[0].BoundGroups)

	require.Nil(t, key.GroupID)
	require.Nil(t, key.Group)
	require.Empty(t, key.BoundGroups)
	require.Empty(t, h.groups.groupLookups, "清空不需要查任何分组")
}

// Create 上的矩阵第 4 行：非 nil 空切片同样优先，落到「未分组 Key」。
func TestAPIKeyCreate_ExplicitEmptyGroupIDsBeatsLegacyGroupID(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name:     "k",
		GroupIDs: groupIDsPtr(),
		GroupID:  ptrInt64(testGroupOpenAI),
	})
	require.NoError(t, err)
	require.Nil(t, key.GroupID)
	require.Nil(t, key.Group)
	require.Empty(t, key.BoundGroups)
	require.Empty(t, h.groups.groupLookups)
}

// 追加要求：默认组 A→B 之后 group_id / group / BoundGroups 三者必须自洽。
// 单独钉一条，防止将来任何一处漂移（T10 的 DTO 直接渲染这三个字段）。
func TestAPIKeyUpdate_DefaultGroupSwitchKeepsGroupIDGroupAndBoundGroupsConsistent(t *testing.T) {
	user := testBindingUser()
	groups := testBindingGroups()
	existing := &APIKey{
		ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive,
		GroupID:     ptrInt64(testGroupAnthropicA),
		Group:       groups[testGroupAnthropicA],
		BoundGroups: []*Group{groups[testGroupAnthropicA], groups[testGroupOpenAI]},
	}
	h := newBindingServiceHarness(t, user, existing)

	// 集合不变，默认组 A(anthropic) → B(openai)。
	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{
		GroupIDs: groupIDsPtr(testGroupAnthropicA, testGroupOpenAI),
		GroupID:  ptrInt64(testGroupOpenAI),
	})
	require.NoError(t, err)
	require.Len(t, h.apiKeys.updated, 1)

	for name, got := range map[string]*APIKey{"返回值": key, "交给仓库层的对象": h.apiKeys.updated[0]} {
		require.NotNil(t, got.GroupID, name)
		require.NotNil(t, got.Group, name+"：group_id 变了，group 不能还是旧对象或 nil")
		require.Equal(t, *got.GroupID, got.Group.ID, name+"：group_id 与 group.id 必须一致")
		require.Contains(t, boundGroupIDs(got.BoundGroups), *got.GroupID,
			name+"：默认组必须同时是绑定集合的成员")
		require.Equal(t, testGroupOpenAI, *got.GroupID, name)
	}
}

// ---------------------------------------------------------------------------
// group_ids 长度上限：自助端点上的查询放大（review Important 2）
// ---------------------------------------------------------------------------

// 上限 = 平台总数。每平台至多一个分组，所以超过它的请求**必然**同平台冲突，
// 合法请求不可能被这条上限挡住。
func TestAPIKeyMaxBoundGroups_CoversEveryBindablePlatform(t *testing.T) {
	require.Equal(t, len(apiKeyBindableGroupPlatforms), apiKeyMaxBoundGroups)

	seen := make(map[string]struct{}, len(apiKeyBindableGroupPlatforms))
	for _, p := range apiKeyBindableGroupPlatforms {
		require.NotContains(t, seen, p, "平台 %s 重复出现，上限会被虚高", p)
		seen[p] = struct{}{}
		require.True(t,
			isConcreteRequestPlatform(p) || p == PlatformComposite || p == PlatformKiro,
			"平台 %s 不是可绑定的分组平台", p)
	}

	// 所有具体请求平台 + composite 都必须在列，否则合法的「绑满所有平台」会被误拒。
	for _, p := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity,
		PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformComposite,
	} {
		require.Contains(t, seen, p, "新增平台后要同步 apiKeyBindableGroupPlatforms")
	}
}

func tooManyGroupIDs() []int64 {
	ids := make([]int64, 0, apiKeyMaxBoundGroups+1)
	for i := 0; i <= apiKeyMaxBoundGroups; i++ {
		// 刻意用互不相同的 id：改造前每一个都会各触发一次分组查询。
		ids = append(ids, int64(900000+i))
	}
	return ids
}

// 超限时**一次分组查询都不能发**。
//
// 这是这条上限存在的全部意义：groupRepo 每个 id 一次查询，普通用户的自助端点上
// 无长度上限就等于一个 N 倍查询放大器。先掐长度、后查库，攻击者拿不到放大倍数。
func TestAPIKeyCreate_RejectsTooManyGroupIDsBeforeAnyRepositoryQuery(t *testing.T) {
	user := testBindingUser()
	h := newBindingServiceHarness(t, user, nil)

	ids := tooManyGroupIDs()
	key, err := h.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: &ids,
	})
	require.ErrorIs(t, err, ErrAPIKeyTooManyBoundGroups)
	require.Nil(t, key)
	require.Empty(t, h.groups.groupLookups, "超限必须在任何仓库查询之前被拒")
	require.Empty(t, h.apiKeys.created)
	require.Zero(t, h.apiKeys.replaceBindingCalls)
	require.Empty(t, h.authCache.deleteAuthKeys)

	// 恰好等于上限时不被这条规则拦下（真正的拒绝理由应该是分组不存在或同平台冲突，
	// 而不是长度）—— 否则上限就会误伤合法请求。
	capped := ids[:apiKeyMaxBoundGroups]
	h2 := newBindingServiceHarness(t, testBindingUser(), nil)
	_, err = h2.svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name: "k", GroupIDs: &capped,
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAPIKeyTooManyBoundGroups)
	require.NotEmpty(t, h2.groups.groupLookups, "上限之内必须照常解析分组")
}

func TestAPIKeyUpdate_RejectsTooManyGroupIDsBeforeAnyRepositoryQuery(t *testing.T) {
	user := testBindingUser()
	existing := &APIKey{ID: 101, UserID: user.ID, Key: "sk-existing", Status: StatusActive}
	h := newBindingServiceHarness(t, user, existing)

	ids := tooManyGroupIDs()
	key, err := h.svc.Update(context.Background(), existing.ID, user.ID, UpdateAPIKeyRequest{GroupIDs: &ids})
	require.ErrorIs(t, err, ErrAPIKeyTooManyBoundGroups)
	require.Nil(t, key)
	require.Empty(t, h.groups.groupLookups, "超限必须在任何仓库查询之前被拒")
	require.Empty(t, h.apiKeys.updated)
	require.Zero(t, h.apiKeys.replaceBindingCalls)
	require.Empty(t, h.authCache.deleteAuthKeys)
}

// ---------------------------------------------------------------------------
// 排序规则只有一份（review Minor 4）
// ---------------------------------------------------------------------------

// SortBoundGroups 与 SortGroupBindings 必须产出同一个顺序 —— 两者共用 lessGroupBinding。
// 曾经 SortBoundGroups 内联了自己那份 (Platform, ID) 比较，注释却声称 lessGroupBinding
// 是「唯一排序规则」；两份实现里任何一处漂移都会让「BoundGroups[0] 就是默认组」失效。
func TestSortBoundGroups_ProducesSameOrderAsSortGroupBindings(t *testing.T) {
	groups := testBindingGroups()
	set := []*Group{
		groups[testGroupOpenAI], groups[testGroupComposite], groups[testGroupAnthropicB],
		groups[testGroupGemini], groups[testGroupAnthropicA],
	}

	bindings := GroupBindingsFromGroups(set) // 内部已 SortGroupBindings
	SortBoundGroups(set)

	require.Len(t, bindings, len(set))
	for i := range set {
		require.Equal(t, bindings[i].GroupID, set[i].ID, "第 %d 个元素的顺序不一致", i)
		require.Equal(t, bindings[i].Platform, set[i].Platform)
	}

	// 默认组恰好是这个序的第一个。
	got := ResolveDefaultGroupIDFromGroups(set)
	require.NotNil(t, got)
	require.Equal(t, set[0].ID, *got)
}

// nil 元素的防御性处理必须保留：排到最后，且不影响非 nil 元素之间的相对顺序。
func TestSortBoundGroups_KeepsNilElementsLast(t *testing.T) {
	groups := testBindingGroups()
	set := []*Group{nil, groups[testGroupOpenAI], nil, groups[testGroupAnthropicA]}
	SortBoundGroups(set)

	require.NotNil(t, set[0])
	require.Equal(t, testGroupAnthropicA, set[0].ID)
	require.NotNil(t, set[1])
	require.Equal(t, testGroupOpenAI, set[1].ID)
	require.Nil(t, set[2])
	require.Nil(t, set[3])
}
