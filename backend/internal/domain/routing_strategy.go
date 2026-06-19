package domain

// 路由策略相关常量。
const (
	// RoutingStrategyActionRestrict 硬路由：命中后只能使用指定账号。
	RoutingStrategyActionRestrict = "restrict"
	// RoutingStrategyActionPrefer 软优先：优先使用指定账号，不可用时回退到全量账号。
	RoutingStrategyActionPrefer = "prefer"

	// RoutingStrategyMatchModeAll 策略内所有条件都满足才算命中。
	RoutingStrategyMatchModeAll = "all"
	// RoutingStrategyMatchModeAny 策略内任一条件满足即算命中。
	RoutingStrategyMatchModeAny = "any"

	// 条件类型。
	RoutingConditionTypeModel     = "model"
	RoutingConditionTypeClient    = "client"
	RoutingConditionTypeUserAgent = "user_agent"

	// model 条件的匹配操作符。
	RoutingConditionOpExact    = "exact"
	RoutingConditionOpWildcard = "wildcard"
	RoutingConditionOpRegex    = "regex"
	// user_agent 条件的匹配操作符。
	RoutingConditionOpContains = "contains"

	// client 条件的取值。
	RoutingClientClaudeCode = "claude_code"
	RoutingClientCodex      = "codex"
	RoutingClientOther      = "other"
	RoutingClientAny        = "any"
)

// RoutingCondition 描述一条路由策略的单个匹配条件。
//
//	Type  : model | client | user_agent
//	Op    : 取决于 Type —— model: exact|wildcard|regex；user_agent: contains|regex；client 无需 Op
//	Value : 待匹配的值（模型模式 / 客户端类型 / UA 子串或正则）
type RoutingCondition struct {
	Type  string `json:"type"`
	Op    string `json:"op,omitempty"`
	Value string `json:"value"`
}
