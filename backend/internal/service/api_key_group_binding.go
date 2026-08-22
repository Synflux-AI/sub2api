package service

import (
	"context"
	"fmt"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 本文件是 issue #171「API Key 绑定多个已有分组」的**绑定集合校验与默认组解析**单一来源。
//
// 分层刻意做成两段：
//   - ValidateGroupBindingSet / ResolveDefaultGroupID 是**纯函数**（不碰 ctx / DB / repo），
//     可以被任何持有 []*Group 的调用方直接复用与单测；
//   - resolveAPIKeyGroupBindingPlan 是服务层入口，负责入参归一化与「ID → *Group」的解析
//     （存在性、软删、用户可见性）之后再套用上面两个纯函数。
//
// 写入约定见 APIKeyUpdateFields.BoundGroups 与 APIKeyRepository.ReplaceBindings：
// 关联表只能整体替换，改默认组时 fields.GroupID 与 fields.BoundGroups 必须同时置位。

// newAPIKeyGroupPlatformConflictError 生成「同平台重复绑定」的错误，
// 错误消息里点名冲突的平台与两个分组，方便用户直接定位要去掉哪一个。
//
// Reason 与 ErrAPIKeyGroupPlatformConflict 一致，因此 errors.Is 仍然成立。
func newAPIKeyGroupPlatformConflictError(platform string, first, second *Group) error {
	return infraerrors.BadRequest(
		apiKeyGroupPlatformConflictReason,
		fmt.Sprintf("平台 %s 上重复绑定了分组「%s」(id=%d) 与「%s」(id=%d)，每个平台最多只能绑定一个分组",
			platform, first.Name, first.ID, second.Name, second.ID),
	)
}

// newAPIKeyCompositeGroupExclusiveError 生成「composite 组与普通组混绑」的错误。
func newAPIKeyCompositeGroupExclusiveError(composite, other *Group) error {
	return infraerrors.BadRequest(
		apiKeyCompositeGroupExclusiveReason,
		fmt.Sprintf("composite 分组「%s」(id=%d) 不能与其它分组混绑（冲突分组：「%s」(id=%d)，平台 %s）",
			composite.Name, composite.ID, other.Name, other.ID, other.Platform),
	)
}

// newAPIKeyDefaultGroupNotBoundError 生成「显式默认组不在绑定集合内」的错误。
func newAPIKeyDefaultGroupNotBoundError(defaultGroupID int64) error {
	return infraerrors.BadRequest(
		apiKeyDefaultGroupNotBoundReason,
		fmt.Sprintf("默认分组 id=%d 不在本次绑定的分组集合内；默认分组必须同时是绑定集合的成员", defaultGroupID),
	)
}

// newAPIKeyTooManyBoundGroupsError 生成「一次绑定的分组数超过上限」的错误。
func newAPIKeyTooManyBoundGroupsError(requested int) error {
	return infraerrors.BadRequest(
		apiKeyTooManyBoundGroupsReason,
		fmt.Sprintf("一次最多只能绑定 %d 个分组（本次请求 %d 个）；每个平台至多一个分组，超过平台总数必然存在同平台冲突",
			apiKeyMaxBoundGroups, requested),
	)
}

// apiKeyBindableGroupPlatforms 列出分组可能取到的全部 platform 值：
// isConcreteRequestPlatform 认可的 8 个具体平台 + composite + 历史遗留的 kiro。
//
// 只用来推导下面的绑定数上限，**不做白名单校验**（分组的 platform 合法性由分组管理侧负责）。
// 新增平台常量时要把它加进来 —— 漏了只会让「一次绑满所有平台」这种极端请求被多拒一个，
// 不会误放任何非法组合。有 TestAPIKeyMaxBoundGroups_CoversEveryBindablePlatform 守着。
//
// 刻意用数组而不是切片：len(数组) 是常量表达式，apiKeyMaxBoundGroups 才能是 const。
var apiKeyBindableGroupPlatforms = [...]string{
	PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity,
	PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek,
	PlatformComposite, PlatformKiro,
}

// apiKeyMaxBoundGroups 是一次请求允许绑定的分组数上限 = 平台总数。
//
// 每个平台至多绑定一个分组（C1），所以「平台总数」就是任何**合法**绑定集合的天然上限：
// 超过它必然存在同平台冲突，请求注定被拒。
//
// 上限检查刻意放在**任何仓库查询之前**：Create / Update 是普通用户的自助端点，
// 若 group_ids 无长度上限，攻击者可以用 N 个互不相同的合法分组 ID 触发 N 次
// groupRepo 查询（放大攻击）。先掐长度，拿不到放大倍数。
const apiKeyMaxBoundGroups = len(apiKeyBindableGroupPlatforms)

// ValidateGroupBindingSet 校验一份「API Key ↔ 分组」绑定集合是否满足 issue #171 的业务不变量。
//
// 纯函数：不读 ctx、不查 DB。调用方负责先把分组 ID 解析成**已存在、未软删、用户有权使用**的 *Group
// （服务层入口见 APIKeyService.resolveAPIKeyGroupBindingPlan）。
//
// 规则：
//  1. 同一平台最多绑定一个分组（C1）。这条同时由 api_key_groups 的
//     (api_key_id, platform) 唯一索引兜底，在这里提前拒绝是为了给出点名两个分组名与平台的
//     可读错误，而不是让用户吃一个 Postgres 23505 转译出来的 ErrAPIKeyGroupBindingConflict。
//  2. composite 分组独占（C2）：绑了 composite 就不能再绑任何其它分组，反之亦然。
//     composite 的跨平台语义与「按命中分组独立计费」互斥。
//
// 空集合合法 —— 对应「未分组 Key」，维持现有语义（C5），不在这里提前报错。
// nil 元素被跳过（与 GroupBindingsFromGroups 一致）；正常调用路径不会产生 nil。
//
// 错误消息由输入顺序决定，因此调用方应先 SortBoundGroups 以获得稳定文案。
func ValidateGroupBindingSet(groups []*Group) error {
	byPlatform := make(map[string]*Group, len(groups))
	var composite, other *Group
	for _, g := range groups {
		if g == nil {
			continue
		}
		if prev, dup := byPlatform[g.Platform]; dup {
			if prev.ID == g.ID {
				// 同一个分组重复出现按去重处理，不算平台冲突。
				continue
			}
			return newAPIKeyGroupPlatformConflictError(g.Platform, prev, g)
		}
		byPlatform[g.Platform] = g
		if g.Platform == PlatformComposite {
			if composite == nil {
				composite = g
			}
			continue
		}
		if other == nil {
			other = g
		}
	}
	if composite != nil && other != nil {
		return newAPIKeyCompositeGroupExclusiveError(composite, other)
	}
	return nil
}

// ResolveDefaultGroupID 按**稳定规则**从绑定集合里选出默认组：
// 先比 platform 字典序（升序），platform 相同再比 group id（升序），取第一个。
//
// 空集合返回 nil —— 未分组 Key 没有默认组（C5）。
//
// 这是全仓**唯一**的默认组解析实现：Create/Update 的默认组解析（T3）与
// 「删掉默认组后从剩余绑定组改选新默认组」（T7）必须都走这里，禁止写第二份。
// 规则本身必须是纯函数且与输入顺序无关，这样同一份绑定集合在任何调用点都得到同一个默认组。
//
// 不修改入参（不做就地排序），调用方可以放心传入共享切片。
func ResolveDefaultGroupID(bindings []GroupBinding) *int64 {
	var best *GroupBinding
	for i := range bindings {
		candidate := &bindings[i]
		if best == nil || lessGroupBinding(*candidate, *best) {
			best = candidate
		}
	}
	if best == nil {
		return nil
	}
	id := best.GroupID
	return &id
}

// ResolveDefaultGroupIDFromGroups 是 ResolveDefaultGroupID 的便利包装：
// 先用 GroupBindingsFromGroups 派生绑定，再走**同一份**排序规则。不是第二份实现。
func ResolveDefaultGroupIDFromGroups(groups []*Group) *int64 {
	return ResolveDefaultGroupID(GroupBindingsFromGroups(groups))
}

// apiKeyGroupBindingPlan 是一次 Create / Update 解析出来的分组绑定写入计划。
type apiKeyGroupBindingPlan struct {
	// Groups 是校验通过的完整绑定集合，已按 (Platform, ID) 稳定排序。
	// 空（nil）表示未分组 Key —— 关联表要被清空。
	Groups []*Group
	// DefaultGroupID 写入 api_keys.group_id。非 nil 时必然是 Groups 的成员
	// （spec §1：「默认分组必须同时存在于关联表」）。
	DefaultGroupID *int64
	// DefaultGroup 是 DefaultGroupID 指向的分组对象，用于回填返回值里的 APIKey.Group，
	// 避免响应里出现「group_id 已经改了、group 还是旧对象」的自相矛盾。
	DefaultGroup *Group
}

// apiKeyGroupBindingMode 区分一次请求到底想干什么。
type apiKeyGroupBindingMode int

const (
	// apiKeyBindingModeReplaceSet：用 GroupIDs 整体替换绑定集合（GroupIDs 为空 = 清空）。
	apiKeyBindingModeReplaceSet apiKeyGroupBindingMode = iota
	// apiKeyBindingModeDefaultOnly：绑定集合**保持不动**，只把默认组换成 DefaultGroupID。
	// 用于「旧客户端只发单值 group_id、而这把 Key 已经绑了多个分组」这一种情况。
	apiKeyBindingModeDefaultOnly
)

// apiKeyGroupBindingIntent 是归一化后的绑定意图。
type apiKeyGroupBindingIntent struct {
	Mode     apiKeyGroupBindingMode
	GroupIDs []int64
	// DefaultGroupID 是**显式指定**的默认组；nil 表示按 ResolveDefaultGroupID 规则解析。
	// Mode == apiKeyBindingModeDefaultOnly 时必然非 nil。
	DefaultGroupID *int64
}

// resolveAPIKeyGroupBindingIntent 把「新的 group_ids 多值入参 + 旧的 group_id 单值入参
// + 这把 Key 当前的绑定数」归一化成一个明确的意图。纯函数。
//
// 完整语义矩阵（Create 传 existingCount = 0）：
//
//	| group_ids     | group_id | 现有绑定数 | 行为                                            |
//	| ------------- | -------- | ---------- | ----------------------------------------------- |
//	| nil（缺省）   | X        | <= 1       | 整体替换成 [X]（**与改造前的单分组语义逐字相同**）|
//	| nil（缺省）   | X        | >= 2       | 只改默认组：集合不动，默认组 = X；X 必须 ∈ 现有集合 |
//	| nil（缺省）   | nil      | 任意       | 空集合（Create = 未分组 Key；Update 不进这个分支）|
//	| 非 nil 空切片 | 任意     | 任意       | **清空全部绑定**（显式解绑优先，不回退旧字段）    |
//	| 非空          | nil      | 任意       | 集合 = group_ids，默认组按稳定规则解析            |
//	| 非空          | X        | 任意       | 集合 = group_ids，默认组 = X；X 必须 ∈ 集合       |
//
// 第 2 行是为了不静默丢数据：handler 无条件回传 group_id（连「只改个名字」的请求都带），
// 所以不能把「只发了单值 group_id」一律当成「把绑定集合缩成一个组」——
// 那会让多分组 Key 被旧客户端一次编辑就丢掉其它平台的绑定。
//
// 第 4 行是为了让「移除所有分组」能被表达出来：非 nil 的空切片是**显式**意图，
// 必须优先于同时带上的旧 group_id，否则用户点「清空」会被静默改回旧默认组。
func resolveAPIKeyGroupBindingIntent(groupIDs *[]int64, legacyGroupID *int64, existingCount int) apiKeyGroupBindingIntent {
	if groupIDs != nil {
		if len(*groupIDs) == 0 {
			// 显式清空：丢掉旧的单值字段，绑定集合与默认组一起清空。
			return apiKeyGroupBindingIntent{Mode: apiKeyBindingModeReplaceSet}
		}
		return apiKeyGroupBindingIntent{
			Mode:           apiKeyBindingModeReplaceSet,
			GroupIDs:       *groupIDs,
			DefaultGroupID: legacyGroupID,
		}
	}
	if legacyGroupID == nil {
		return apiKeyGroupBindingIntent{Mode: apiKeyBindingModeReplaceSet}
	}
	if existingCount >= 2 {
		return apiKeyGroupBindingIntent{
			Mode:           apiKeyBindingModeDefaultOnly,
			DefaultGroupID: legacyGroupID,
		}
	}
	return apiKeyGroupBindingIntent{
		Mode:           apiKeyBindingModeReplaceSet,
		GroupIDs:       []int64{*legacyGroupID},
		DefaultGroupID: legacyGroupID,
	}
}

// resolveAPIKeyGroupBindingPlan 是 Create / Update **唯一**的绑定解析入口。
//
// groupIDs 三态：nil = 请求没带 group_ids；非 nil 空切片 = 显式清空；非空 = 整体替换。
// existing 是这把 Key 当前的绑定集合（Create 传 nil），只用于上面矩阵第 2 行的判定。
func (s *APIKeyService) resolveAPIKeyGroupBindingPlan(
	ctx context.Context,
	user *User,
	groupIDs *[]int64,
	legacyGroupID *int64,
	existing []*Group,
) (apiKeyGroupBindingPlan, error) {
	intent := resolveAPIKeyGroupBindingIntent(groupIDs, legacyGroupID, len(existing))
	if intent.Mode == apiKeyBindingModeDefaultOnly {
		// DefaultGroupID 在 DefaultOnly 模式下必然非 nil（只有 legacyGroupID != nil 才会进这个模式）。
		// 这条不变量由 TestResolveAPIKeyGroupBindingIntent_DefaultOnlyAlwaysCarriesDefaultGroupID 钉住，
		// 所以这里直接解引用，而不是留一个从本入口不可达的防御分支。
		return planAPIKeyDefaultGroupChange(existing, *intent.DefaultGroupID)
	}
	return s.planAPIKeyBindingSetReplacement(ctx, user, intent.GroupIDs, intent.DefaultGroupID)
}

// planAPIKeyDefaultGroupChange 处理「集合不动、只改默认组」。纯内存，不查库。
//
// 刻意**不**重新做 canUserBindGroup 校验：本路径不新增任何绑定，
// 重校验只会让「分组权限事后被撤销」的用户连改默认组都做不了。
// 真要变更集合必须走 group_ids，那条路径每个分组都会重新校验。
func planAPIKeyDefaultGroupChange(existing []*Group, defaultGroupID int64) (apiKeyGroupBindingPlan, error) {
	var plan apiKeyGroupBindingPlan

	groups := make([]*Group, 0, len(existing))
	var target *Group
	for _, g := range existing {
		if g == nil {
			continue
		}
		groups = append(groups, g)
		if g.ID == defaultGroupID {
			target = g
		}
	}
	if target == nil {
		return plan, newAPIKeyDefaultGroupNotBoundError(defaultGroupID)
	}

	SortBoundGroups(groups)
	// 集合本身没变，这里校验是为了在读模型万一已经破裂时给出可读的 400，
	// 而不是让 ReplaceBindings 去撞 (api_key_id, platform) 唯一索引吃 409。
	if err := ValidateGroupBindingSet(groups); err != nil {
		return plan, err
	}

	id := target.ID
	plan.Groups = groups
	plan.DefaultGroupID = &id
	plan.DefaultGroup = target
	return plan, nil
}

// planAPIKeyBindingSetReplacement 处理「整体替换绑定集合」。
//
// groupIDs 顺序无关，重复 ID 自动去重（同一个分组给两次不算平台冲突）；空 = 清空绑定（C5）。
// explicitDefaultGroupID 非 nil 时必须出现在 groupIDs 里。
//
// 校验顺序刻意与改造前的单分组路径保持一致 —— 逐个查分组
// （分组不存在或已软删都返回 ErrGroupNotFound，软删由 ent 的软删拦截器保证），
// 紧跟 canUserBindGroup（专属标准组走 User.CanBindGroup，订阅组要求有效订阅）。
// 这样单分组请求产生的错误与包装文案与改造前逐字相同（C3）。
//
// 用 GetByIDLite 而不是 GetByID：绑定只需要分组自身配置，不需要 account 统计
// （GetByID 会额外跑一次 loadAccountCounts）。读路径回填的 BoundGroups 同样不带统计，
// 所以这也让写入返回值与读路径的形状一致。
func (s *APIKeyService) planAPIKeyBindingSetReplacement(
	ctx context.Context,
	user *User,
	groupIDs []int64,
	explicitDefaultGroupID *int64,
) (apiKeyGroupBindingPlan, error) {
	var plan apiKeyGroupBindingPlan

	if len(groupIDs) == 0 {
		// 空集合是合法输入：维持「未分组 Key」语义（C5）。
		// 归一化保证此时 explicitDefaultGroupID 一定是 nil（显式清空会丢掉旧字段）。
		return plan, nil
	}
	// 长度上限必须在任何仓库查询之前检查，见 apiKeyMaxBoundGroups 的说明。
	if len(groupIDs) > apiKeyMaxBoundGroups {
		return plan, newAPIKeyTooManyBoundGroupsError(len(groupIDs))
	}

	seen := make(map[int64]*Group, len(groupIDs))
	groups := make([]*Group, 0, len(groupIDs))
	for _, gid := range groupIDs {
		if _, dup := seen[gid]; dup {
			continue
		}
		group, err := s.groupRepo.GetByIDLite(ctx, gid)
		if err != nil {
			return plan, fmt.Errorf("get group: %w", err)
		}
		if !s.canUserBindGroup(ctx, user, group) {
			return plan, ErrGroupNotAllowed
		}
		seen[gid] = group
		groups = append(groups, group)
	}

	SortBoundGroups(groups)
	if err := ValidateGroupBindingSet(groups); err != nil {
		return plan, err
	}

	if explicitDefaultGroupID != nil {
		defaultGroup, ok := seen[*explicitDefaultGroupID]
		if !ok {
			return plan, newAPIKeyDefaultGroupNotBoundError(*explicitDefaultGroupID)
		}
		id := *explicitDefaultGroupID
		plan.DefaultGroupID = &id
		plan.DefaultGroup = defaultGroup
	} else {
		plan.DefaultGroupID = ResolveDefaultGroupIDFromGroups(groups)
		if plan.DefaultGroupID != nil {
			plan.DefaultGroup = seen[*plan.DefaultGroupID]
		}
	}

	plan.Groups = groups
	return plan, nil
}
