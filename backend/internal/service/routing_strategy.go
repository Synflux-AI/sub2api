package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 路由策略动作与匹配相关常量（从 domain 导出便于上层复用）。
const (
	RoutingActionRestrict = domain.RoutingStrategyActionRestrict
	RoutingActionPrefer   = domain.RoutingStrategyActionPrefer

	RoutingMatchModeAll = domain.RoutingStrategyMatchModeAll
	RoutingMatchModeAny = domain.RoutingStrategyMatchModeAny

	RoutingConditionTypeModel     = domain.RoutingConditionTypeModel
	RoutingConditionTypeClient    = domain.RoutingConditionTypeClient
	RoutingConditionTypeUserAgent = domain.RoutingConditionTypeUserAgent

	RoutingConditionOpExact    = domain.RoutingConditionOpExact
	RoutingConditionOpWildcard = domain.RoutingConditionOpWildcard
	RoutingConditionOpRegex    = domain.RoutingConditionOpRegex
	RoutingConditionOpContains = domain.RoutingConditionOpContains

	RoutingClientClaudeCode = domain.RoutingClientClaudeCode
	RoutingClientCodex      = domain.RoutingClientCodex
	RoutingClientOther      = domain.RoutingClientOther
	RoutingClientAny        = domain.RoutingClientAny
)

// 路由策略相关错误。
var (
	ErrRoutingStrategyNotFound  = infraerrors.NotFound("ROUTING_STRATEGY_NOT_FOUND", "routing strategy not found")
	ErrRoutingStrategyNilInput  = infraerrors.BadRequest("ROUTING_STRATEGY_INPUT_REQUIRED", "routing strategy input is required")
	ErrRoutingStrategyName      = infraerrors.BadRequest("ROUTING_STRATEGY_NAME_INVALID", "routing strategy name is invalid")
	ErrRoutingStrategyAction    = infraerrors.BadRequest("ROUTING_STRATEGY_ACTION_INVALID", "routing strategy action must be restrict or prefer")
	ErrRoutingStrategyMatchMode = infraerrors.BadRequest("ROUTING_STRATEGY_MATCH_MODE_INVALID", "routing strategy match_mode must be all or any")
	ErrRoutingStrategyAccounts  = infraerrors.BadRequest("ROUTING_STRATEGY_ACCOUNTS_REQUIRED", "routing strategy requires at least one account")
	ErrRoutingStrategyCondition = infraerrors.BadRequest("ROUTING_STRATEGY_CONDITION_INVALID", "routing strategy has an invalid condition")
)

// RoutingCondition 是 domain.RoutingCondition 的别名。
type RoutingCondition = domain.RoutingCondition

// RoutingStrategy 是路由策略的运行时模型。
type RoutingStrategy struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Enabled     bool               `json:"enabled"`
	Priority    int                `json:"priority"`
	Platform    string             `json:"platform"`
	GroupID     *int64             `json:"group_id"`
	MatchMode   string             `json:"match_mode"`
	Conditions  []RoutingCondition `json:"conditions"`
	Action      string             `json:"action"`
	AccountIDs  []int64            `json:"account_ids"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// RoutingStrategyRepository 定义路由策略持久化接口。
type RoutingStrategyRepository interface {
	Create(ctx context.Context, s *RoutingStrategy) error
	GetByID(ctx context.Context, id int64) (*RoutingStrategy, error)
	Update(ctx context.Context, s *RoutingStrategy) error
	Delete(ctx context.Context, id int64) error
	// List 返回全部未删除策略，按 priority 升序、id 升序。
	List(ctx context.Context) ([]RoutingStrategy, error)
	// ListEnabled 仅返回启用策略，按 priority 升序、id 升序（热路径使用）。
	ListEnabled(ctx context.Context) ([]RoutingStrategy, error)
}
