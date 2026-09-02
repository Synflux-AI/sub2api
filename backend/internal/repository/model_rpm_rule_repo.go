package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 模型维度 RPM 规则仓储。
//
// 与 user_group_rate_multipliers 一致走原生 SQL（表由 234_model_rpm_rules.sql 建立，不进 ent schema）。
// 列表查询顺带 LEFT JOIN 出分组名/用户名，供管理台直接展示 target，避免前端 N+1 查询。
const modelRPMRuleSelectColumns = `
	r.id, r.name, r.model_pattern, r.scope, r.target_type, r.target_id,
	r.rpm_limit, r.enabled, r.created_at, r.updated_at,
	COALESCE(g.name, u.username, '')
`

const modelRPMRuleFromClause = `
	FROM model_rpm_rules r
	LEFT JOIN groups g ON r.target_type = 'group' AND g.id = r.target_id
	LEFT JOIN users u ON r.target_type = 'user' AND u.id = r.target_id
`

type modelRPMRuleRepository struct {
	sql sqlExecutor
}

// NewModelRPMRuleRepository 创建模型 RPM 规则仓储。
func NewModelRPMRuleRepository(sqlDB *sql.DB) service.ModelRPMRuleRepository {
	return &modelRPMRuleRepository{sql: sqlDB}
}

func scanModelRPMRule(scan func(dest ...any) error) (service.ModelRPMRule, error) {
	var (
		rule       service.ModelRPMRule
		targetID   sql.NullInt64
		createdAt  time.Time
		updatedAt  time.Time
		targetName string
	)
	if err := scan(
		&rule.ID, &rule.Name, &rule.ModelPattern, &rule.Scope, &rule.TargetType, &targetID,
		&rule.RPMLimit, &rule.Enabled, &createdAt, &updatedAt, &targetName,
	); err != nil {
		return service.ModelRPMRule{}, err
	}
	if targetID.Valid {
		id := targetID.Int64
		rule.TargetID = &id
	}
	rule.CreatedAt = createdAt.Format(time.RFC3339)
	rule.UpdatedAt = updatedAt.Format(time.RFC3339)
	rule.TargetName = targetName
	return rule, nil
}

// ListAll 返回全部规则（含停用），按 id 升序。
func (r *modelRPMRuleRepository) ListAll(ctx context.Context) ([]service.ModelRPMRule, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT`+modelRPMRuleSelectColumns+modelRPMRuleFromClause+`ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	rules := make([]service.ModelRPMRule, 0)
	for rows.Next() {
		rule, scanErr := scanModelRPMRule(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}

// GetByID 返回单条规则，不存在时返回 service.ErrModelRPMRuleNotFound。
func (r *modelRPMRuleRepository) GetByID(ctx context.Context, id int64) (*service.ModelRPMRule, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT`+modelRPMRuleSelectColumns+modelRPMRuleFromClause+`WHERE r.id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrModelRPMRuleNotFound
	}
	rule, err := scanModelRPMRule(rows.Scan)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &rule, nil
}

// Create 插入规则并回填 id / 时间戳。
func (r *modelRPMRuleRepository) Create(ctx context.Context, rule *service.ModelRPMRule) error {
	if rule == nil {
		return service.ErrModelRPMRuleNilInput
	}
	var (
		id        int64
		createdAt time.Time
		updatedAt time.Time
	)
	err := scanSingleRow(ctx, r.sql, `
		INSERT INTO model_rpm_rules (name, model_pattern, scope, target_type, target_id, rpm_limit, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, updated_at
	`, []any{rule.Name, rule.ModelPattern, rule.Scope, rule.TargetType, nullableTargetID(rule.TargetID), rule.RPMLimit, rule.Enabled},
		&id, &createdAt, &updatedAt)
	if err != nil {
		return translatePersistenceError(err, nil, service.ErrModelRPMRuleConflict)
	}
	rule.ID = id
	rule.CreatedAt = createdAt.Format(time.RFC3339)
	rule.UpdatedAt = updatedAt.Format(time.RFC3339)
	return nil
}

// Update 全量更新规则。
func (r *modelRPMRuleRepository) Update(ctx context.Context, rule *service.ModelRPMRule) error {
	if rule == nil {
		return service.ErrModelRPMRuleNilInput
	}
	var updatedAt time.Time
	err := scanSingleRow(ctx, r.sql, `
		UPDATE model_rpm_rules
		SET name = $2, model_pattern = $3, scope = $4, target_type = $5, target_id = $6,
			rpm_limit = $7, enabled = $8, updated_at = NOW()
		WHERE id = $1
		RETURNING updated_at
	`, []any{rule.ID, rule.Name, rule.ModelPattern, rule.Scope, rule.TargetType, nullableTargetID(rule.TargetID), rule.RPMLimit, rule.Enabled},
		&updatedAt)
	if err != nil {
		return translatePersistenceError(err, service.ErrModelRPMRuleNotFound, service.ErrModelRPMRuleConflict)
	}
	rule.UpdatedAt = updatedAt.Format(time.RFC3339)
	return nil
}

// Delete 删除规则；目标不存在时返回 service.ErrModelRPMRuleNotFound。
func (r *modelRPMRuleRepository) Delete(ctx context.Context, id int64) error {
	result, err := r.sql.ExecContext(ctx, `DELETE FROM model_rpm_rules WHERE id = $1`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrModelRPMRuleNotFound
	}
	return nil
}

func nullableTargetID(targetID *int64) any {
	if targetID == nil {
		return nil
	}
	return *targetID
}
