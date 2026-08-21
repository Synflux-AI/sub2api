package service

import (
	"context"
	"errors"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// AdminUpdateAPIKeyGroupIDs 是管理端的**多分组**绑定接口（issue #171）。
//
// 与只收单个 group_id 的 AdminUpdateAPIKeyGroupID 的关系：
//   - 那个是遗留形状，语义等价于「只发了旧 group_id」，走的是「现有绑定 <=1 整体替换 /
//     >=2 只改默认组」那套矩阵，管理员用它没法真正变更多分组 Key 的集合；
//   - 这个接受完整集合，是管理端表单多选的落点。
//
// 不变量与用户端**完全共用同一份实现**（ValidateGroupBindingSet +
// ResolveDefaultGroupIDFromGroups），不写第二份规则：
// 每平台至多一个分组、composite 不与普通组混绑、默认组必在集合内。
//
// 管理端特有的一点：绑定专属标准分组时自动给用户授权（与
// AdminUpdateAPIKeyGroupID 的既有行为一致），并且整个过程在一个事务里 ——
// 「授权」与「写绑定」必须原子，否则会留下「Key 绑了组但用户没权限」的状态，
// 下一次认证就 403。
func (s *adminServiceImpl) AdminUpdateAPIKeyGroupIDs(ctx context.Context, keyID int64, groupIDs []int64) (*AdminUpdateAPIKeyGroupIDResult, error) {
	apiKey, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}

	// 长度上限先于任何查询，理由同 apiKeyMaxBoundGroups：这是个可被调用方控制
	// 长度的入参，不掐会变成查询放大器。
	if len(groupIDs) > apiKeyMaxBoundGroups {
		return nil, newAPIKeyTooManyBoundGroupsError(len(groupIDs))
	}

	result := &AdminUpdateAPIKeyGroupIDResult{}

	// 空集合 = 解绑全部。与 AdminUpdateAPIKeyGroupID 的 group_id=0 等价。
	if len(groupIDs) == 0 {
		apiKey.GroupID = nil
		apiKey.Group = nil
		apiKey.BoundGroups = nil
		if err := s.apiKeyRepo.Update(ctx, apiKey, APIKeyUpdateFields{GroupID: true, BoundGroups: true}); err != nil {
			return nil, fmt.Errorf("update api key: %w", err)
		}
		if s.authCacheInvalidator != nil {
			s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
		}
		result.APIKey = apiKey
		return result, nil
	}

	seen := make(map[int64]struct{}, len(groupIDs))
	groups := make([]*Group, 0, len(groupIDs))
	var exclusiveStandard []int64
	for _, gid := range groupIDs {
		if gid <= 0 {
			return nil, infraerrors.BadRequest("INVALID_GROUP_ID", "group_id must be positive")
		}
		if _, dup := seen[gid]; dup {
			continue
		}
		seen[gid] = struct{}{}

		group, err := s.groupRepo.GetByID(ctx, gid)
		if err != nil {
			return nil, err
		}
		if group.Status != StatusActive {
			return nil, infraerrors.BadRequest("GROUP_NOT_ACTIVE", fmt.Sprintf("target group is not active: %d", gid))
		}
		// 订阅类型分组：用户须持有该分组的有效订阅才可绑定（与单值接口同一规则）。
		if group.IsSubscriptionType() {
			if s.userSubRepo == nil {
				return nil, infraerrors.InternalServer("SUBSCRIPTION_REPOSITORY_UNAVAILABLE", "subscription repository is not configured")
			}
			if _, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, apiKey.UserID, gid); err != nil {
				if errors.Is(err, ErrSubscriptionNotFound) {
					return nil, infraerrors.BadRequest("SUBSCRIPTION_REQUIRED",
						fmt.Sprintf("user does not have an active subscription for group %d", gid))
				}
				return nil, err
			}
		} else if group.IsExclusive {
			exclusiveStandard = append(exclusiveStandard, gid)
		}
		groups = append(groups, group)
	}

	SortBoundGroups(groups)
	// 与用户端同一份不变量校验：同平台冲突 / composite 混绑。
	if err := ValidateGroupBindingSet(groups); err != nil {
		return nil, err
	}

	defaultGroupID := ResolveDefaultGroupIDFromGroups(groups)
	apiKey.GroupID = defaultGroupID
	apiKey.BoundGroups = groups
	apiKey.Group = nil
	if defaultGroupID != nil {
		for _, g := range groups {
			if g.ID == *defaultGroupID {
				apiKey.Group = g
				break
			}
		}
	}

	// 「自动授权专属分组」与「写绑定」必须同一事务。
	opCtx := ctx
	var tx *dbent.Tx
	if len(exclusiveStandard) > 0 {
		if s.entClient == nil {
			logger.LegacyPrintf("service.admin", "Warning: entClient is nil, skipping transaction protection for exclusive group binding")
		} else {
			var txErr error
			tx, txErr = s.entClient.Tx(ctx)
			if txErr != nil {
				return nil, fmt.Errorf("begin transaction: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			opCtx = dbent.NewTxContext(ctx, tx)
		}
		for _, gid := range exclusiveStandard {
			if addErr := s.userRepo.AddGroupToAllowedGroups(opCtx, apiKey.UserID, gid); addErr != nil {
				return nil, fmt.Errorf("add group to user allowed groups: %w", addErr)
			}
		}
		result.AutoGrantedGroupAccess = true
		// 与单值接口保持一致：只回报其中一个（前端只用它做提示文案）。
		granted := exclusiveStandard[0]
		result.GrantedGroupID = &granted
		for _, g := range groups {
			if g.ID == granted {
				result.GrantedGroupName = g.Name
				break
			}
		}
	}

	if err := s.apiKeyRepo.Update(opCtx, apiKey, APIKeyUpdateFields{GroupID: true, BoundGroups: true}); err != nil {
		return nil, fmt.Errorf("update api key: %w", err)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit transaction: %w", err)
		}
	}

	// 缓存失效放在事务提交之后。这里是 key 级失效：只有这一把 Key 的快照变了
	// （组级失效属于删组/改平台，见 group_repo）。
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	}

	result.APIKey = apiKey
	return result, nil
}
