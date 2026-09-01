package service

import (
	"context"
	"fmt"
)

// ModelRPMRuleService 管理端模型 RPM 规则 CRUD。
//
// 写入后主动失效本副本的规则快照；多副本靠 resolver 的 TTL 最终一致，
// 因此改规则在其它副本上最长 30s 生效。
type ModelRPMRuleService struct {
	repo      ModelRPMRuleRepository
	resolver  *ModelRPMRuleResolver
	groupRepo GroupRepository
	userRepo  UserRepository
}

// NewModelRPMRuleService 创建规则管理服务。
func NewModelRPMRuleService(
	repo ModelRPMRuleRepository,
	resolver *ModelRPMRuleResolver,
	groupRepo GroupRepository,
	userRepo UserRepository,
) *ModelRPMRuleService {
	return &ModelRPMRuleService{repo: repo, resolver: resolver, groupRepo: groupRepo, userRepo: userRepo}
}

// List 返回全部规则（含停用），按 id 升序。
func (s *ModelRPMRuleService) List(ctx context.Context) ([]ModelRPMRule, error) {
	if s == nil || s.repo == nil {
		return nil, ErrModelRPMRuleNilInput
	}
	return s.repo.ListAll(ctx)
}

// GetByID 返回单条规则。
func (s *ModelRPMRuleService) GetByID(ctx context.Context, id int64) (*ModelRPMRule, error) {
	if s == nil || s.repo == nil {
		return nil, ErrModelRPMRuleNilInput
	}
	return s.repo.GetByID(ctx, id)
}

// Create 创建规则。
func (s *ModelRPMRuleService) Create(ctx context.Context, input *SaveModelRPMRuleInput) (*ModelRPMRule, error) {
	if s == nil || s.repo == nil {
		return nil, ErrModelRPMRuleNilInput
	}
	rule, err := s.normalize(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, rule); err != nil {
		return nil, err
	}
	s.invalidate()
	return rule, nil
}

// Update 全量更新规则。
func (s *ModelRPMRuleService) Update(ctx context.Context, id int64, input *SaveModelRPMRuleInput) (*ModelRPMRule, error) {
	if s == nil || s.repo == nil {
		return nil, ErrModelRPMRuleNilInput
	}
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	rule, err := s.normalize(ctx, input)
	if err != nil {
		return nil, err
	}
	rule.ID = existing.ID
	rule.CreatedAt = existing.CreatedAt
	if err := s.repo.Update(ctx, rule); err != nil {
		return nil, err
	}
	s.invalidate()
	return rule, nil
}

// Delete 删除规则。
func (s *ModelRPMRuleService) Delete(ctx context.Context, id int64) error {
	if s == nil || s.repo == nil {
		return ErrModelRPMRuleNilInput
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func (s *ModelRPMRuleService) invalidate() {
	if s.resolver != nil {
		s.resolver.Invalidate()
	}
}

// normalize 归一化输入并校验 target 是否真实存在。
// 目标不存在时直接拒绝：一条永不命中的规则在管理台上看起来是生效的，属于静默失效。
func (s *ModelRPMRuleService) normalize(ctx context.Context, input *SaveModelRPMRuleInput) (*ModelRPMRule, error) {
	rule, err := NormalizeAndValidateModelRPMRule(input)
	if err != nil {
		return nil, err
	}

	switch rule.TargetType {
	case ModelRPMTargetGroup:
		if s.groupRepo == nil {
			break
		}
		group, err := s.groupRepo.GetByIDLite(ctx, *rule.TargetID)
		if err != nil {
			return nil, fmt.Errorf("resolve rule target group: %w", err)
		}
		if group == nil {
			return nil, ErrModelRPMRuleTargetID
		}
		rule.TargetName = group.Name
	case ModelRPMTargetUser:
		if s.userRepo == nil {
			break
		}
		user, err := s.userRepo.GetByID(ctx, *rule.TargetID)
		if err != nil {
			return nil, fmt.Errorf("resolve rule target user: %w", err)
		}
		if user == nil {
			return nil, ErrModelRPMRuleTargetID
		}
		rule.TargetName = user.Username
	}

	return rule, nil
}
