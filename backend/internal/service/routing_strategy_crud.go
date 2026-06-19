package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// SaveRoutingStrategyInput 是创建/更新路由策略的输入（全量替换语义）。
type SaveRoutingStrategyInput struct {
	Name        string
	Description string
	Enabled     bool
	Priority    int
	Platform    string
	GroupID     *int64
	MatchMode   string
	Conditions  []RoutingCondition
	Action      string
	AccountIDs  []int64
}

// List 返回全部策略（按 priority 升序）。
func (s *RoutingStrategyService) List(ctx context.Context) ([]RoutingStrategy, error) {
	return s.repo.List(ctx)
}

// GetByID 返回单个策略。
func (s *RoutingStrategyService) GetByID(ctx context.Context, id int64) (*RoutingStrategy, error) {
	return s.repo.GetByID(ctx, id)
}

// Create 创建策略。
func (s *RoutingStrategyService) Create(ctx context.Context, input *SaveRoutingStrategyInput) (*RoutingStrategy, error) {
	st, err := normalizeAndValidateRoutingStrategy(input)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, st); err != nil {
		return nil, fmt.Errorf("create routing strategy: %w", err)
	}
	s.invalidate()
	return st, nil
}

// Update 全量更新策略。
func (s *RoutingStrategyService) Update(ctx context.Context, id int64, input *SaveRoutingStrategyInput) (*RoutingStrategy, error) {
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	st, err := normalizeAndValidateRoutingStrategy(input)
	if err != nil {
		return nil, err
	}
	st.ID = existing.ID
	st.CreatedAt = existing.CreatedAt
	if err := s.repo.Update(ctx, st); err != nil {
		return nil, fmt.Errorf("update routing strategy: %w", err)
	}
	s.invalidate()
	return st, nil
}

// Delete 删除策略。
func (s *RoutingStrategyService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete routing strategy: %w", err)
	}
	s.invalidate()
	return nil
}

// TestRoutingDecision 对给定请求属性进行 dry-run 评估，返回命中的策略与决策。
// 不读取缓存以外的数据，纯函数式预演，便于管理端验证配置。
func (s *RoutingStrategyService) TestRoutingDecision(ctx context.Context, mc RoutingMatchContext) RoutingDecision {
	return s.Evaluate(ctx, mc)
}

func normalizeAndValidateRoutingStrategy(input *SaveRoutingStrategyInput) (*RoutingStrategy, error) {
	if input == nil {
		return nil, ErrRoutingStrategyNilInput
	}

	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 128 {
		return nil, ErrRoutingStrategyName
	}

	action := strings.TrimSpace(input.Action)
	if action == "" {
		action = RoutingActionRestrict
	}
	if action != RoutingActionRestrict && action != RoutingActionPrefer {
		return nil, ErrRoutingStrategyAction
	}

	matchMode := strings.TrimSpace(input.MatchMode)
	if matchMode == "" {
		matchMode = RoutingMatchModeAll
	}
	if matchMode != RoutingMatchModeAll && matchMode != RoutingMatchModeAny {
		return nil, ErrRoutingStrategyMatchMode
	}

	accountIDs := dedupInt64(input.AccountIDs)
	if len(accountIDs) == 0 {
		return nil, ErrRoutingStrategyAccounts
	}

	conditions, err := normalizeConditions(input.Conditions)
	if err != nil {
		return nil, err
	}

	priority := input.Priority
	if priority < 0 {
		priority = 0
	}

	var groupID *int64
	if input.GroupID != nil && *input.GroupID > 0 {
		v := *input.GroupID
		groupID = &v
	}

	return &RoutingStrategy{
		Name:        name,
		Description: strings.TrimSpace(input.Description),
		Enabled:     input.Enabled,
		Priority:    priority,
		Platform:    strings.TrimSpace(input.Platform),
		GroupID:     groupID,
		MatchMode:   matchMode,
		Conditions:  conditions,
		Action:      action,
		AccountIDs:  accountIDs,
	}, nil
}

func normalizeConditions(in []RoutingCondition) ([]RoutingCondition, error) {
	out := make([]RoutingCondition, 0, len(in))
	for i := range in {
		c := RoutingCondition{
			Type:  strings.TrimSpace(in[i].Type),
			Op:    strings.TrimSpace(in[i].Op),
			Value: strings.TrimSpace(in[i].Value),
		}
		switch c.Type {
		case RoutingConditionTypeModel:
			if c.Op == "" {
				c.Op = RoutingConditionOpWildcard
			}
			if c.Op != RoutingConditionOpExact && c.Op != RoutingConditionOpWildcard && c.Op != RoutingConditionOpRegex {
				return nil, ErrRoutingStrategyCondition
			}
			if c.Value == "" {
				return nil, ErrRoutingStrategyCondition
			}
			if c.Op == RoutingConditionOpRegex {
				if _, err := regexp.Compile(c.Value); err != nil {
					return nil, ErrRoutingStrategyCondition
				}
			}
		case RoutingConditionTypeUserAgent:
			if c.Op == "" {
				c.Op = RoutingConditionOpContains
			}
			if c.Op != RoutingConditionOpContains && c.Op != RoutingConditionOpRegex {
				return nil, ErrRoutingStrategyCondition
			}
			if c.Value == "" {
				return nil, ErrRoutingStrategyCondition
			}
			if c.Op == RoutingConditionOpRegex {
				if _, err := regexp.Compile(c.Value); err != nil {
					return nil, ErrRoutingStrategyCondition
				}
			}
		case RoutingConditionTypeClient:
			c.Op = ""
			switch c.Value {
			case RoutingClientClaudeCode, RoutingClientCodex, RoutingClientOther, RoutingClientAny:
			default:
				return nil, ErrRoutingStrategyCondition
			}
		default:
			return nil, ErrRoutingStrategyCondition
		}
		out = append(out, c)
	}
	return out, nil
}
