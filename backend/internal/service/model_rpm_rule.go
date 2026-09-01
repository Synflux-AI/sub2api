package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 模型维度 RPM 限流规则的取值域。
//
// scope 与 target_type 正交，不要混淆：
//   - target_type 决定「这条规则管谁」（全部用户 / 某分组 / 某用户）。
//   - scope 决定「配额怎么分」（每个用户各一份 / 命中范围内共享一池）。
//
// 四种组合都合法，例如 scope=user + target_type=group 表示
// 「该分组内每个用户对该模型各有一份独立配额」。
const (
	ModelRPMScopeUser   = "user"
	ModelRPMScopeGlobal = "global"

	ModelRPMTargetAll   = "all"
	ModelRPMTargetGroup = "group"
	ModelRPMTargetUser  = "user"
)

// 规则校验错误。管理端 handler 通过 response.ErrorFrom 映射为 400。
var (
	ErrModelRPMRuleNilInput   = infraerrors.BadRequest("MODEL_RPM_RULE_INVALID", "model rpm rule input is required")
	ErrModelRPMRuleName       = infraerrors.BadRequest("MODEL_RPM_RULE_NAME_INVALID", "rule name is required and must be at most 128 characters")
	ErrModelRPMRulePattern    = infraerrors.BadRequest("MODEL_RPM_RULE_PATTERN_INVALID", "model pattern is required and supports at most one trailing '*'")
	ErrModelRPMRuleScope      = infraerrors.BadRequest("MODEL_RPM_RULE_SCOPE_INVALID", "scope must be one of: user, global")
	ErrModelRPMRuleTargetType = infraerrors.BadRequest("MODEL_RPM_RULE_TARGET_TYPE_INVALID", "target_type must be one of: all, group, user")
	ErrModelRPMRuleTargetID   = infraerrors.BadRequest("MODEL_RPM_RULE_TARGET_ID_INVALID", "target_id is required when target_type is group or user")
	// rpm_limit 必须为正：本表不提供 user_group_rate_multipliers.rpm_override=0 那样的「免检绿灯」语义。
	// 两个 RPM 概念对 0 的含义相反，前端文案需明确区分。
	ErrModelRPMRuleLimit    = infraerrors.BadRequest("MODEL_RPM_RULE_LIMIT_INVALID", "rpm_limit must be a positive integer")
	ErrModelRPMRuleNotFound = infraerrors.NotFound("MODEL_RPM_RULE_NOT_FOUND", "model rpm rule not found")
	ErrModelRPMRuleConflict = infraerrors.Conflict("MODEL_RPM_RULE_CONFLICT", "an enabled rule with the same model pattern, scope and target already exists")
)

// ModelRPMRule 是一条模型维度的 RPM 限流规则。
type ModelRPMRule struct {
	ID int64 `json:"id"`
	// Name 仅用于管理台辨认，不参与匹配。
	Name string `json:"name"`
	// ModelPattern 是客户端请求体里的公开模型名（已归一化为 trim + 小写），
	// 支持尾部 `*` 前缀通配，例如 `claude-opus-*`。
	ModelPattern string `json:"model_pattern"`
	Scope        string `json:"scope"`
	TargetType   string `json:"target_type"`
	// TargetID 在 TargetType 为 group/user 时非 nil。
	TargetID  *int64 `json:"target_id,omitempty"`
	RPMLimit  int    `json:"rpm_limit"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`

	// TargetName 是管理端列表用的展示字段（分组名 / 用户名），不入库。
	TargetName string `json:"target_name,omitempty"`
}

// SaveModelRPMRuleInput 是创建/更新规则的输入（全量替换语义）。
type SaveModelRPMRuleInput struct {
	Name         string
	ModelPattern string
	Scope        string
	TargetType   string
	TargetID     *int64
	RPMLimit     int
	Enabled      bool
}

// ModelRPMRuleRepository 模型 RPM 规则仓储接口。
type ModelRPMRuleRepository interface {
	// ListAll 返回全部规则（含停用），按 id 升序。读路径的内存快照与管理台列表共用。
	ListAll(ctx context.Context) ([]ModelRPMRule, error)
	GetByID(ctx context.Context, id int64) (*ModelRPMRule, error)
	Create(ctx context.Context, rule *ModelRPMRule) error
	Update(ctx context.Context, rule *ModelRPMRule) error
	Delete(ctx context.Context, id int64) error
}

// ModelRPMCache 模型维度 RPM 计数器接口。
//
// 与 UserRPMCache 的区别：桶按「规则」而非「用户/分组」聚合。
// 用 ruleID 而非模型名做 key 是刻意的：模型名含 `[1m]`、`/` 等字符；
// 通配规则也没有单一模型名可用；用 ruleID 还能让「改限额」立即生效，
// 而「改模型匹配」自然换桶。
type ModelRPMCache interface {
	// MinuteTimestamp 返回 Redis 服务端当前分钟戳。
	//
	// 分钟戳单独成一个方法（而非藏在 Increment 内部）是为了让同一请求命中的多条规则
	// 共用一次 TIME 调用：既省 RTT，也避免多条规则跨分钟边界落进不同窗口。
	MinuteTimestamp(ctx context.Context) (minute int64, err error)

	// IncrementRuleRPM 原子递增指定规则在给定分钟窗口的计数并返回最新值。
	// userID <= 0 表示 scope=global，走全站共享的单一桶。
	IncrementRuleRPM(ctx context.Context, ruleID, userID, minute int64) (count int, err error)

	// GetRuleRPM 读取指定规则当前分钟的已用计数（只读，不递增）。
	GetRuleRPM(ctx context.Context, ruleID, userID, minute int64) (count int, err error)
}

// ModelRPMRuleSnapshotProvider 提供内存中的规则快照。
// 由 modelRPMRuleResolver 实现；checkRPM 只依赖这个只读视图。
type ModelRPMRuleSnapshotProvider interface {
	Snapshot(ctx context.Context) []ModelRPMRule
}

// NormalizeModelRPMPattern 归一化模型匹配串：trim + 小写。
// 公开模型名大小写不敏感，配置与请求走同一套归一化，避免「大小写不同就漏判」。
func NormalizeModelRPMPattern(pattern string) string {
	return strings.ToLower(strings.TrimSpace(pattern))
}

// MatchesModel 判断规则是否匹配给定的公开模型名。
// model 必须已经过 NormalizeModelRPMPattern 归一化。
func (r ModelRPMRule) MatchesModel(model string) bool {
	if model == "" || r.ModelPattern == "" {
		return false
	}
	if strings.HasSuffix(r.ModelPattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(r.ModelPattern, "*"))
	}
	return r.ModelPattern == model
}

// MatchesTarget 判断规则的适用范围是否覆盖当前 (用户, 分组)。
func (r ModelRPMRule) MatchesTarget(userID int64, group *Group) bool {
	switch r.TargetType {
	case ModelRPMTargetAll:
		return true
	case ModelRPMTargetGroup:
		return r.TargetID != nil && group != nil && group.ID == *r.TargetID
	case ModelRPMTargetUser:
		return r.TargetID != nil && userID > 0 && userID == *r.TargetID
	default:
		return false
	}
}

// targetSpecificity 给 target 具体度打分：user > group > all。
// 用于确定性排序，让「首次超限即返回」在多规则命中时行为可预期。
func (r ModelRPMRule) targetSpecificity() int {
	switch r.TargetType {
	case ModelRPMTargetUser:
		return 2
	case ModelRPMTargetGroup:
		return 1
	default:
		return 0
	}
}

// NormalizeAndValidateModelRPMRule 归一化并校验管理端输入。
func NormalizeAndValidateModelRPMRule(input *SaveModelRPMRuleInput) (*ModelRPMRule, error) {
	if input == nil {
		return nil, ErrModelRPMRuleNilInput
	}

	name := strings.TrimSpace(input.Name)
	if name == "" || len([]rune(name)) > 128 {
		return nil, ErrModelRPMRuleName
	}

	pattern := NormalizeModelRPMPattern(input.ModelPattern)
	if pattern == "" || len(pattern) > 256 {
		return nil, ErrModelRPMRulePattern
	}
	// 只允许「最多一个尾部 *」：中缀通配会让匹配语义与 Redis 分桶都难以解释。
	if idx := strings.Index(pattern, "*"); idx >= 0 && idx != len(pattern)-1 {
		return nil, ErrModelRPMRulePattern
	}
	if pattern == "*" {
		return nil, ErrModelRPMRulePattern
	}

	scope := strings.ToLower(strings.TrimSpace(input.Scope))
	if scope == "" {
		scope = ModelRPMScopeUser
	}
	if scope != ModelRPMScopeUser && scope != ModelRPMScopeGlobal {
		return nil, ErrModelRPMRuleScope
	}

	targetType := strings.ToLower(strings.TrimSpace(input.TargetType))
	if targetType == "" {
		targetType = ModelRPMTargetAll
	}
	if targetType != ModelRPMTargetAll && targetType != ModelRPMTargetGroup && targetType != ModelRPMTargetUser {
		return nil, ErrModelRPMRuleTargetType
	}

	var targetID *int64
	if targetType == ModelRPMTargetAll {
		// target_type=all 时忽略传入的 target_id，与库上的 CHECK 约束保持一致。
		targetID = nil
	} else {
		if input.TargetID == nil || *input.TargetID <= 0 {
			return nil, ErrModelRPMRuleTargetID
		}
		id := *input.TargetID
		targetID = &id
	}

	if input.RPMLimit <= 0 {
		return nil, ErrModelRPMRuleLimit
	}

	return &ModelRPMRule{
		Name:         name,
		ModelPattern: pattern,
		Scope:        scope,
		TargetType:   targetType,
		TargetID:     targetID,
		RPMLimit:     input.RPMLimit,
		Enabled:      input.Enabled,
	}, nil
}
