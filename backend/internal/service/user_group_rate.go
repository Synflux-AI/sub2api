package service

import "context"

// UserGroupRateEntry 分组下用户专属倍率/RPM 条目。
// RateMultiplier 与 RPMOverride 均为指针以支持"未设置"语义（NULL）。
type UserGroupRateEntry struct {
	UserID         int64    `json:"user_id"`
	UserName       string   `json:"user_name"`
	UserEmail      string   `json:"user_email"`
	UserNotes      string   `json:"user_notes"`
	UserStatus     string   `json:"user_status"`
	RateMultiplier *float64 `json:"rate_multiplier,omitempty"`
	RPMOverride    *int     `json:"rpm_override,omitempty"`
	// ModelRateMultipliers 用户对分组模型倍率规则的覆盖：分组规则模式串 -> 倍率；未出现的模式串继承分组。
	ModelRateMultipliers map[string]float64 `json:"model_rate_multipliers,omitempty"`
}

// UserGroupRateConfig 计费热路径一次读出的用户分组倍率配置。
type UserGroupRateConfig struct {
	RateMultiplier       *float64           // 用户专属倍率；nil 表示未设置
	ModelRateMultipliers map[string]float64 // 模型倍率覆盖，键为分组规则模式串
}

// UserGroupModelRateRepository 用户在分组下的模型倍率覆盖读写。
// 作为可选能力由 SQL 仓储实现，调用方以类型断言获取（与 GetByUserIDs 的批量读取同一做法），
// 未实现时计费只按用户专属倍率解析、管理接口返回不可用。
type UserGroupModelRateRepository interface {
	// GetRateConfigByUserAndGroup 一次读出专属倍率与模型倍率覆盖；无行时返回 nil
	GetRateConfigByUserAndGroup(ctx context.Context, userID, groupID int64) (*UserGroupRateConfig, error)
	// SetUserGroupModelRates 整体替换用户在分组下的模型倍率覆盖；空 map 表示全部恢复继承
	SetUserGroupModelRates(ctx context.Context, userID, groupID int64, rates map[string]float64) error
	// PruneGroupModelRates 删除该分组所有用户覆盖中不在 patterns 里的模式串
	PruneGroupModelRates(ctx context.Context, groupID int64, patterns []string) error
}

// GroupRateMultiplierInput 批量设置分组倍率的输入条目
type GroupRateMultiplierInput struct {
	UserID         int64   `json:"user_id"`
	RateMultiplier float64 `json:"rate_multiplier"`
}

// GroupRPMOverrideInput 批量设置分组 RPM override 的输入条目。
// RPMOverride 为 *int 以支持清除（nil）语义。
type GroupRPMOverrideInput struct {
	UserID      int64 `json:"user_id"`
	RPMOverride *int  `json:"rpm_override"`
}

// UserGroupRateRepository 用户专属分组倍率/RPM 仓储接口。
// 允许管理员为特定用户设置分组的专属计费倍率与 RPM 上限，覆盖分组默认值。
type UserGroupRateRepository interface {
	// GetByUserID 获取用户所有专属分组 rate_multiplier（仅返回非 NULL 的条目）
	GetByUserID(ctx context.Context, userID int64) (map[int64]float64, error)

	// GetByUserAndGroup 获取用户在特定分组的专属 rate_multiplier（NULL 返回 nil）
	GetByUserAndGroup(ctx context.Context, userID, groupID int64) (*float64, error)

	// GetRPMOverrideByUserAndGroup 获取用户在特定分组的 rpm_override（NULL 返回 nil）
	GetRPMOverrideByUserAndGroup(ctx context.Context, userID, groupID int64) (*int, error)

	// GetByGroupID 获取指定分组下所有用户的专属配置（rate 与 rpm_override 任一非 NULL 即返回）
	GetByGroupID(ctx context.Context, groupID int64) ([]UserGroupRateEntry, error)

	// SyncUserGroupRates 同步用户的分组专属倍率；nil 表示清空该分组的 rate_multiplier
	SyncUserGroupRates(ctx context.Context, userID int64, rates map[int64]*float64) error

	// SyncGroupRateMultipliers 批量同步分组的用户专属倍率（替换整组 rate 部分）
	SyncGroupRateMultipliers(ctx context.Context, groupID int64, entries []GroupRateMultiplierInput) error

	// SyncGroupRPMOverrides 批量同步分组的用户专属 RPM（替换整组 rpm_override 部分）。
	// 条目中 RPMOverride 为 nil 时清空对应行的 rpm_override；非 nil 时 upsert。
	SyncGroupRPMOverrides(ctx context.Context, groupID int64, entries []GroupRPMOverrideInput) error

	// ClearGroupRPMOverrides 清空指定分组的所有 rpm_override（整组 rpm 部分归 NULL）
	ClearGroupRPMOverrides(ctx context.Context, groupID int64) error

	// DeleteByGroupID 删除指定分组的所有用户专属条目（分组删除时调用）
	DeleteByGroupID(ctx context.Context, groupID int64) error

	// DeleteByUserID 删除指定用户的所有专属条目（用户删除时调用）
	DeleteByUserID(ctx context.Context, userID int64) error
}
