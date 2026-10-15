package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type userGroupRateRepository struct {
	sql sqlExecutor
}

// NewUserGroupRateRepository 创建用户专属分组倍率/RPM 仓储
func NewUserGroupRateRepository(sqlDB *sql.DB) service.UserGroupRateRepository {
	return &userGroupRateRepository{sql: sqlDB}
}

// GetByUserID 获取用户所有专属分组 rate_multiplier（仅返回非 NULL 的条目）
func (r *userGroupRateRepository) GetByUserID(ctx context.Context, userID int64) (map[int64]float64, error) {
	query := `SELECT group_id, rate_multiplier FROM user_group_rate_multipliers WHERE user_id = $1 AND rate_multiplier IS NOT NULL`
	rows, err := r.sql.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]float64)
	for rows.Next() {
		var groupID int64
		var rate float64
		if err := rows.Scan(&groupID, &rate); err != nil {
			return nil, err
		}
		result[groupID] = rate
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetByUserIDs 批量获取多个用户的专属分组 rate_multiplier（仅返回非 NULL 的条目）
func (r *userGroupRateRepository) GetByUserIDs(ctx context.Context, userIDs []int64) (map[int64]map[int64]float64, error) {
	result := make(map[int64]map[int64]float64, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}

	uniqueIDs := make([]int64, 0, len(userIDs))
	seen := make(map[int64]struct{}, len(userIDs))
	for _, userID := range userIDs {
		if userID <= 0 {
			continue
		}
		if _, exists := seen[userID]; exists {
			continue
		}
		seen[userID] = struct{}{}
		uniqueIDs = append(uniqueIDs, userID)
		result[userID] = make(map[int64]float64)
	}
	if len(uniqueIDs) == 0 {
		return result, nil
	}

	rows, err := r.sql.QueryContext(ctx, `
		SELECT user_id, group_id, rate_multiplier
		FROM user_group_rate_multipliers
		WHERE user_id = ANY($1) AND rate_multiplier IS NOT NULL
	`, pq.Array(uniqueIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var userID int64
		var groupID int64
		var rate float64
		if err := rows.Scan(&userID, &groupID, &rate); err != nil {
			return nil, err
		}
		if _, ok := result[userID]; !ok {
			result[userID] = make(map[int64]float64)
		}
		result[userID][groupID] = rate
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetByGroupID 获取指定分组下所有用户的专属配置（rate、rpm_override、模型倍率覆盖任一非空即有行）
func (r *userGroupRateRepository) GetByGroupID(ctx context.Context, groupID int64) ([]service.UserGroupRateEntry, error) {
	query := `
		SELECT ugr.user_id, u.username, u.email, COALESCE(u.notes, ''), u.status, ugr.rate_multiplier, ugr.rpm_override, ugr.model_rate_multipliers
		FROM user_group_rate_multipliers ugr
		JOIN users u ON u.id = ugr.user_id AND u.deleted_at IS NULL
		WHERE ugr.group_id = $1
		ORDER BY ugr.user_id
	`
	rows, err := r.sql.QueryContext(ctx, query, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var result []service.UserGroupRateEntry
	for rows.Next() {
		var entry service.UserGroupRateEntry
		var rate sql.NullFloat64
		var rpm sql.NullInt32
		var modelRates []byte
		if err := rows.Scan(&entry.UserID, &entry.UserName, &entry.UserEmail, &entry.UserNotes, &entry.UserStatus, &rate, &rpm, &modelRates); err != nil {
			return nil, err
		}
		if entry.ModelRateMultipliers, err = decodeUserGroupModelRates(modelRates); err != nil {
			return nil, err
		}
		if rate.Valid {
			v := rate.Float64
			entry.RateMultiplier = &v
		}
		if rpm.Valid {
			v := int(rpm.Int32)
			entry.RPMOverride = &v
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetByUserAndGroup 获取用户在特定分组的专属 rate_multiplier（NULL 返回 nil）
func (r *userGroupRateRepository) GetByUserAndGroup(ctx context.Context, userID, groupID int64) (*float64, error) {
	query := `SELECT rate_multiplier FROM user_group_rate_multipliers WHERE user_id = $1 AND group_id = $2`
	var rate sql.NullFloat64
	err := scanSingleRow(ctx, r.sql, query, []any{userID, groupID}, &rate)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !rate.Valid {
		return nil, nil
	}
	v := rate.Float64
	return &v, nil
}

// GetRPMOverrideByUserAndGroup 获取用户在特定分组的 rpm_override（NULL 返回 nil）
func (r *userGroupRateRepository) GetRPMOverrideByUserAndGroup(ctx context.Context, userID, groupID int64) (*int, error) {
	query := `SELECT rpm_override FROM user_group_rate_multipliers WHERE user_id = $1 AND group_id = $2`
	var rpm sql.NullInt32
	err := scanSingleRow(ctx, r.sql, query, []any{userID, groupID}, &rpm)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !rpm.Valid {
		return nil, nil
	}
	v := int(rpm.Int32)
	return &v, nil
}

// SyncUserGroupRates 同步用户的分组专属 rate_multiplier。
//   - 传入空 map：清空该用户所有行的 rate_multiplier；若 rpm_override 也为 NULL 则整行删除。
//   - 值为 nil：清空对应行的 rate_multiplier（保留 rpm_override）。
//   - 值非 nil：upsert rate_multiplier（保留已有 rpm_override）。
func (r *userGroupRateRepository) SyncUserGroupRates(ctx context.Context, userID int64, rates map[int64]*float64) error {
	if len(rates) == 0 {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET rate_multiplier = NULL, updated_at = NOW()
			WHERE user_id = $1
		`, userID); err != nil {
			return err
		}
		_, err := r.sql.ExecContext(ctx,
			`DELETE FROM user_group_rate_multipliers WHERE user_id = $1 AND rate_multiplier IS NULL AND rpm_override IS NULL AND model_rate_multipliers = '{}'::jsonb`,
			userID)
		return err
	}

	var clearGroupIDs []int64
	upsertGroupIDs := make([]int64, 0, len(rates))
	upsertRates := make([]float64, 0, len(rates))
	for groupID, rate := range rates {
		if rate == nil {
			clearGroupIDs = append(clearGroupIDs, groupID)
		} else {
			upsertGroupIDs = append(upsertGroupIDs, groupID)
			upsertRates = append(upsertRates, *rate)
		}
	}

	if len(clearGroupIDs) > 0 {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET rate_multiplier = NULL, updated_at = NOW()
			WHERE user_id = $1 AND group_id = ANY($2)
		`, userID, pq.Array(clearGroupIDs)); err != nil {
			return err
		}
		if _, err := r.sql.ExecContext(ctx,
			`DELETE FROM user_group_rate_multipliers WHERE user_id = $1 AND group_id = ANY($2) AND rate_multiplier IS NULL AND rpm_override IS NULL AND model_rate_multipliers = '{}'::jsonb`,
			userID, pq.Array(clearGroupIDs)); err != nil {
			return err
		}
	}

	if len(upsertGroupIDs) > 0 {
		now := time.Now()
		_, err := r.sql.ExecContext(ctx, `
			INSERT INTO user_group_rate_multipliers (user_id, group_id, rate_multiplier, created_at, updated_at)
			SELECT
				$1::bigint,
				data.group_id,
				data.rate_multiplier,
				$2::timestamptz,
				$2::timestamptz
			FROM unnest($3::bigint[], $4::double precision[]) AS data(group_id, rate_multiplier)
			ON CONFLICT (user_id, group_id)
			DO UPDATE SET
				rate_multiplier = EXCLUDED.rate_multiplier,
				updated_at = EXCLUDED.updated_at
		`, userID, now, pq.Array(upsertGroupIDs), pq.Array(upsertRates))
		if err != nil {
			return err
		}
	}

	return nil
}

// SyncGroupRateMultipliers 同步分组的 rate_multiplier 部分（不触动 rpm_override）。
// 语义：
//   - 未出现在 entries 中的用户行：rate_multiplier 归 NULL；若 rpm_override 也为 NULL 则整行删除。
//   - 出现的用户行：upsert rate_multiplier。
func (r *userGroupRateRepository) SyncGroupRateMultipliers(ctx context.Context, groupID int64, entries []service.GroupRateMultiplierInput) error {
	keepUserIDs := make([]int64, 0, len(entries))
	for _, e := range entries {
		keepUserIDs = append(keepUserIDs, e.UserID)
	}

	// 未在 entries 列表中的行：清空 rate_multiplier。
	if len(keepUserIDs) == 0 {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET rate_multiplier = NULL, updated_at = NOW()
			WHERE group_id = $1
		`, groupID); err != nil {
			return err
		}
	} else {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET rate_multiplier = NULL, updated_at = NOW()
			WHERE group_id = $1 AND user_id <> ALL($2)
		`, groupID, pq.Array(keepUserIDs)); err != nil {
			return err
		}
	}

	// 清空后若整行 NULL 则删除。
	if _, err := r.sql.ExecContext(ctx, `
		DELETE FROM user_group_rate_multipliers
		WHERE group_id = $1 AND rate_multiplier IS NULL AND rpm_override IS NULL AND model_rate_multipliers = '{}'::jsonb
	`, groupID); err != nil {
		return err
	}

	if len(entries) == 0 {
		return nil
	}

	userIDs := make([]int64, len(entries))
	rates := make([]float64, len(entries))
	for i, e := range entries {
		userIDs[i] = e.UserID
		rates[i] = e.RateMultiplier
	}
	now := time.Now()
	_, err := r.sql.ExecContext(ctx, `
		INSERT INTO user_group_rate_multipliers (user_id, group_id, rate_multiplier, created_at, updated_at)
		SELECT data.user_id, $1::bigint, data.rate_multiplier, $2::timestamptz, $2::timestamptz
		FROM unnest($3::bigint[], $4::double precision[]) AS data(user_id, rate_multiplier)
		ON CONFLICT (user_id, group_id)
		DO UPDATE SET rate_multiplier = EXCLUDED.rate_multiplier, updated_at = EXCLUDED.updated_at
	`, groupID, now, pq.Array(userIDs), pq.Array(rates))
	return err
}

// SyncGroupRPMOverrides 同步分组的 rpm_override 部分（不触动 rate_multiplier）。
// 语义：
//   - 未出现的用户行：rpm_override 归 NULL；若 rate_multiplier 也为 NULL 则整行删除。
//   - 出现的用户行：若 RPMOverride 为 nil 则清空；非 nil 则 upsert。
func (r *userGroupRateRepository) SyncGroupRPMOverrides(ctx context.Context, groupID int64, entries []service.GroupRPMOverrideInput) error {
	keepUserIDs := make([]int64, 0, len(entries))
	var clearUserIDs []int64
	upsertUserIDs := make([]int64, 0, len(entries))
	upsertValues := make([]int32, 0, len(entries))
	for _, e := range entries {
		keepUserIDs = append(keepUserIDs, e.UserID)
		if e.RPMOverride == nil {
			clearUserIDs = append(clearUserIDs, e.UserID)
		} else {
			upsertUserIDs = append(upsertUserIDs, e.UserID)
			upsertValues = append(upsertValues, int32(*e.RPMOverride))
		}
	}

	// 未在 entries 列表中的行：清空 rpm_override。
	if len(keepUserIDs) == 0 {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET rpm_override = NULL, updated_at = NOW()
			WHERE group_id = $1
		`, groupID); err != nil {
			return err
		}
	} else {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET rpm_override = NULL, updated_at = NOW()
			WHERE group_id = $1 AND user_id <> ALL($2)
		`, groupID, pq.Array(keepUserIDs)); err != nil {
			return err
		}
	}

	// 显式 clear 的行。
	if len(clearUserIDs) > 0 {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET rpm_override = NULL, updated_at = NOW()
			WHERE group_id = $1 AND user_id = ANY($2)
		`, groupID, pq.Array(clearUserIDs)); err != nil {
			return err
		}
	}

	// 清空后若整行 NULL 则删除。
	if _, err := r.sql.ExecContext(ctx, `
		DELETE FROM user_group_rate_multipliers
		WHERE group_id = $1 AND rate_multiplier IS NULL AND rpm_override IS NULL AND model_rate_multipliers = '{}'::jsonb
	`, groupID); err != nil {
		return err
	}

	if len(upsertUserIDs) > 0 {
		now := time.Now()
		_, err := r.sql.ExecContext(ctx, `
			INSERT INTO user_group_rate_multipliers (user_id, group_id, rpm_override, created_at, updated_at)
			SELECT data.user_id, $1::bigint, data.rpm_override, $2::timestamptz, $2::timestamptz
			FROM unnest($3::bigint[], $4::integer[]) AS data(user_id, rpm_override)
			ON CONFLICT (user_id, group_id)
			DO UPDATE SET rpm_override = EXCLUDED.rpm_override, updated_at = EXCLUDED.updated_at
		`, groupID, now, pq.Array(upsertUserIDs), pq.Array(upsertValues))
		if err != nil {
			return err
		}
	}

	return nil
}

// ClearGroupRPMOverrides 清空指定分组所有行的 rpm_override。
func (r *userGroupRateRepository) ClearGroupRPMOverrides(ctx context.Context, groupID int64) error {
	if _, err := r.sql.ExecContext(ctx, `
		UPDATE user_group_rate_multipliers
		SET rpm_override = NULL, updated_at = NOW()
		WHERE group_id = $1
	`, groupID); err != nil {
		return err
	}
	_, err := r.sql.ExecContext(ctx, `
		DELETE FROM user_group_rate_multipliers
		WHERE group_id = $1 AND rate_multiplier IS NULL AND rpm_override IS NULL AND model_rate_multipliers = '{}'::jsonb
	`, groupID)
	return err
}

// DeleteByGroupID 删除指定分组的所有用户专属条目
func (r *userGroupRateRepository) DeleteByGroupID(ctx context.Context, groupID int64) error {
	_, err := r.sql.ExecContext(ctx, `DELETE FROM user_group_rate_multipliers WHERE group_id = $1`, groupID)
	return err
}

// DeleteByUserID 删除指定用户的所有专属条目
func (r *userGroupRateRepository) DeleteByUserID(ctx context.Context, userID int64) error {
	_, err := r.sql.ExecContext(ctx, `DELETE FROM user_group_rate_multipliers WHERE user_id = $1`, userID)
	return err
}

// GetRateConfigByUserAndGroup 一次读出用户在分组下的专属倍率与模型倍率覆盖；无行时返回 nil。
func (r *userGroupRateRepository) GetRateConfigByUserAndGroup(ctx context.Context, userID, groupID int64) (*service.UserGroupRateConfig, error) {
	query := `SELECT rate_multiplier, model_rate_multipliers FROM user_group_rate_multipliers WHERE user_id = $1 AND group_id = $2`
	var rate sql.NullFloat64
	var modelRates []byte
	err := scanSingleRow(ctx, r.sql, query, []any{userID, groupID}, &rate, &modelRates)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &service.UserGroupRateConfig{}
	if rate.Valid {
		v := rate.Float64
		cfg.RateMultiplier = &v
	}
	if cfg.ModelRateMultipliers, err = decodeUserGroupModelRates(modelRates); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetUserGroupModelRates 整体替换用户在分组下的模型倍率覆盖（不触动 rate_multiplier 与 rpm_override）。
// 空 map 表示全部恢复继承；替换后若整行为空则删除。
func (r *userGroupRateRepository) SetUserGroupModelRates(ctx context.Context, userID, groupID int64, rates map[string]float64) error {
	payload, err := encodeUserGroupModelRates(rates)
	if err != nil {
		return err
	}
	if len(rates) == 0 {
		if _, err := r.sql.ExecContext(ctx, `
			UPDATE user_group_rate_multipliers
			SET model_rate_multipliers = '{}'::jsonb, updated_at = NOW()
			WHERE user_id = $1 AND group_id = $2
		`, userID, groupID); err != nil {
			return err
		}
		_, err := r.sql.ExecContext(ctx, `
			DELETE FROM user_group_rate_multipliers
			WHERE user_id = $1 AND group_id = $2 AND rate_multiplier IS NULL AND rpm_override IS NULL AND model_rate_multipliers = '{}'::jsonb
		`, userID, groupID)
		return err
	}
	now := time.Now()
	_, err = r.sql.ExecContext(ctx, `
		INSERT INTO user_group_rate_multipliers (user_id, group_id, model_rate_multipliers, created_at, updated_at)
		VALUES ($1, $2, $3::jsonb, $4, $4)
		ON CONFLICT (user_id, group_id)
		DO UPDATE SET model_rate_multipliers = EXCLUDED.model_rate_multipliers, updated_at = EXCLUDED.updated_at
	`, userID, groupID, payload, now)
	return err
}

// PruneGroupModelRates 删除该分组所有用户覆盖里不在 patterns 中的键（分组删改模型规则后调用），
// 随后清理整行为空的记录。patterns 必须是分组规则里原样保存的模式串。
func (r *userGroupRateRepository) PruneGroupModelRates(ctx context.Context, groupID int64, patterns []string) error {
	if patterns == nil {
		patterns = []string{}
	}
	if _, err := r.sql.ExecContext(ctx, `
		UPDATE user_group_rate_multipliers
		SET model_rate_multipliers = COALESCE((
				SELECT jsonb_object_agg(kv.key, kv.value)
				FROM jsonb_each(model_rate_multipliers) AS kv
				WHERE kv.key = ANY($2)
			), '{}'::jsonb),
			updated_at = NOW()
		WHERE group_id = $1
			AND model_rate_multipliers <> '{}'::jsonb
			AND EXISTS (
				SELECT 1 FROM jsonb_object_keys(model_rate_multipliers) AS k
				WHERE NOT (k = ANY($2))
			)
	`, groupID, pq.Array(patterns)); err != nil {
		return err
	}
	_, err := r.sql.ExecContext(ctx, `
		DELETE FROM user_group_rate_multipliers
		WHERE group_id = $1 AND rate_multiplier IS NULL AND rpm_override IS NULL AND model_rate_multipliers = '{}'::jsonb
	`, groupID)
	return err
}

func decodeUserGroupModelRates(raw []byte) (map[string]float64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]float64
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode user group model rate multipliers: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func encodeUserGroupModelRates(rates map[string]float64) (string, error) {
	if len(rates) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(rates)
	if err != nil {
		return "", fmt.Errorf("encode user group model rate multipliers: %w", err)
	}
	return string(b), nil
}

// GetModelRatesByUserID 返回用户在各分组下的模型倍率覆盖（group_id → 模式串 → 倍率），只含非空条目。
func (r *userGroupRateRepository) GetModelRatesByUserID(ctx context.Context, userID int64) (map[int64]map[string]float64, error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT group_id, model_rate_multipliers
		FROM user_group_rate_multipliers
		WHERE user_id = $1 AND model_rate_multipliers <> '{}'::jsonb
	`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]map[string]float64)
	for rows.Next() {
		var groupID int64
		var raw []byte
		if err := rows.Scan(&groupID, &raw); err != nil {
			return nil, err
		}
		rates, err := decodeUserGroupModelRates(raw)
		if err != nil {
			return nil, err
		}
		if len(rates) > 0 {
			result[groupID] = rates
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

var (
	_ service.UserGroupModelRateRepository    = (*userGroupRateRepository)(nil)
	_ service.UserGroupModelRatesByUserReader = (*userGroupRateRepository)(nil)
)
