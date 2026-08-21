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
//   - resolveGroupBindingPlan 是服务层入口，负责「ID → *Group」的解析
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

// ValidateGroupBindingSet 校验一份「API Key ↔ 分组」绑定集合是否满足 issue #171 的业务不变量。
//
// 纯函数：不读 ctx、不查 DB。调用方负责先把分组 ID 解析成**已存在、未软删、用户有权使用**的 *Group
// （服务层入口见 APIKeyService.resolveGroupBindingPlan）。
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

// apiKeyRequestedGroupIDs 把「新的 group_ids 多值入参 + 旧的 group_id 单值入参」
// 归一化成 (绑定集合, 显式默认组) 两个参数，是新旧接口的兼容层。
//
//   - 只给旧 group_id  → 等价于 group_ids = [group_id]，并且它就是默认组；
//   - 只给 group_ids   → 默认组交给 ResolveDefaultGroupID 按稳定规则解析；
//   - 两个都给         → group_ids 是绑定集合，group_id 是**显式指定的默认组**，
//     必须落在集合内，否则 resolveGroupBindingPlan 拒绝。
func apiKeyRequestedGroupIDs(groupIDs []int64, legacyGroupID *int64) ([]int64, *int64) {
	if len(groupIDs) == 0 {
		if legacyGroupID == nil {
			return nil, nil
		}
		return []int64{*legacyGroupID}, legacyGroupID
	}
	return groupIDs, legacyGroupID
}

// resolveAPIKeyGroupBindingPlan 是 Create / Update 唯一的绑定解析入口：
// 先用 apiKeyRequestedGroupIDs 把新旧入参归一化，再走 resolveGroupBindingPlan 校验。
func (s *APIKeyService) resolveAPIKeyGroupBindingPlan(
	ctx context.Context,
	user *User,
	groupIDs []int64,
	legacyGroupID *int64,
) (apiKeyGroupBindingPlan, error) {
	ids, explicitDefaultGroupID := apiKeyRequestedGroupIDs(groupIDs, legacyGroupID)
	return s.resolveGroupBindingPlan(ctx, user, ids, explicitDefaultGroupID)
}

// resolveGroupBindingPlan 是 Create / Update **共用**的多分组校验与默认组解析入口。
//
// groupIDs 顺序无关，重复 ID 自动去重（同一个分组给两次不算平台冲突）。
// explicitDefaultGroupID 非 nil 时必须出现在 groupIDs 里。
//
// 校验顺序刻意与改造前的单分组路径保持一致 —— 逐个 groupRepo.GetByID
// （分组不存在或已软删都返回 ErrGroupNotFound，软删由 ent 的软删拦截器保证），
// 紧跟 canUserBindGroup（专属标准组走 User.CanBindGroup，订阅组要求有效订阅）。
// 这样单分组请求产生的错误与包装文案与改造前逐字相同（C3）。
func (s *APIKeyService) resolveGroupBindingPlan(
	ctx context.Context,
	user *User,
	groupIDs []int64,
	explicitDefaultGroupID *int64,
) (apiKeyGroupBindingPlan, error) {
	var plan apiKeyGroupBindingPlan

	if len(groupIDs) == 0 {
		if explicitDefaultGroupID != nil {
			// 空绑定集合却指定了默认组 —— 默认组无处安放，直接拒绝。
			return plan, newAPIKeyDefaultGroupNotBoundError(*explicitDefaultGroupID)
		}
		// 空集合是合法输入：维持「未分组 Key」语义（C5）。
		return plan, nil
	}

	seen := make(map[int64]*Group, len(groupIDs))
	groups := make([]*Group, 0, len(groupIDs))
	for _, gid := range groupIDs {
		if _, dup := seen[gid]; dup {
			continue
		}
		group, err := s.groupRepo.GetByID(ctx, gid)
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
