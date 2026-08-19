package repository

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetUsageFilterOptions returns only dimension values present in the current
// window and filter intersection, which keeps the admin selectors relational.
func (r *usageLogRepository) GetUsageFilterOptions(ctx context.Context, startTime, endTime time.Time, filters UsageLogFilters) (*usagestats.UsageFilterOptions, error) {
	result := &usagestats.UsageFilterOptions{
		Users: make([]usagestats.UsageFilterUser, 0), Models: make([]string, 0),
		Groups: make([]usagestats.UsageFilterEntity, 0), Accounts: make([]usagestats.UsageFilterEntity, 0),
	}

	where, args := usageFilterOptionsWhere(startTime, endTime, filters, "user")
	rows, err := r.sql.QueryContext(ctx, "SELECT DISTINCT ul.user_id, COALESCE(u.email, ''), COALESCE(u.notes, '') FROM usage_logs ul LEFT JOIN users u ON u.id = ul.user_id"+where+" ORDER BY ul.user_id", args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item usagestats.UsageFilterUser
		if err := rows.Scan(&item.ID, &item.Email, &item.Notes); err != nil {
			rows.Close()
			return nil, err
		}
		result.Users = append(result.Users, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	where, args = usageFilterOptionsWhere(startTime, endTime, filters, "model")
	modelExpr := resolveModelDimensionExpressionWithAlias(usagestats.ModelSourceRequested, "ul")
	rows, err = r.sql.QueryContext(ctx, "SELECT DISTINCT "+modelExpr+" FROM usage_logs ul"+where+" AND "+modelExpr+" <> '' ORDER BY 1", args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			rows.Close()
			return nil, err
		}
		result.Models = append(result.Models, model)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	where, args = usageFilterOptionsWhere(startTime, endTime, filters, "group")
	rows, err = r.sql.QueryContext(ctx, "SELECT DISTINCT ul.group_id, COALESCE(g.name, ''), COALESCE(g.platform, '') FROM usage_logs ul LEFT JOIN groups g ON g.id = ul.group_id"+where+" AND ul.group_id IS NOT NULL ORDER BY 2, 1", args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item usagestats.UsageFilterEntity
		if err := rows.Scan(&item.ID, &item.Name, &item.Platform); err != nil {
			rows.Close()
			return nil, err
		}
		result.Groups = append(result.Groups, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	where, args = usageFilterOptionsWhere(startTime, endTime, filters, "account")
	rows, err = r.sql.QueryContext(ctx, "SELECT DISTINCT ul.account_id, COALESCE(a.name, ''), COALESCE(a.platform, '') FROM usage_logs ul LEFT JOIN accounts a ON a.id = ul.account_id"+where+" ORDER BY 2, 1", args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item usagestats.UsageFilterEntity
		if err := rows.Scan(&item.ID, &item.Name, &item.Platform); err != nil {
			rows.Close()
			return nil, err
		}
		result.Accounts = append(result.Accounts, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}

func usageFilterOptionsWhere(startTime, endTime time.Time, filters UsageLogFilters, exclude string) (string, []any) {
	conditions := []string{"ul.created_at >= $1", "ul.created_at < $2"}
	args := []any{startTime, endTime}
	if exclude != "user" {
		conditions, args = appendUsageLogIDWhereCondition(conditions, args, "ul.user_id", filters.UserID, filters.UserIDs)
	}
	conditions, args = appendUsageLogIDWhereCondition(conditions, args, "ul.api_key_id", filters.APIKeyID, filters.APIKeyIDs)
	if exclude != "account" {
		conditions, args = appendUsageLogIDWhereCondition(conditions, args, "ul.account_id", filters.AccountID, filters.AccountIDs)
	}
	if exclude != "group" {
		conditions, args = appendUsageLogIDWhereCondition(conditions, args, "ul.group_id", filters.GroupID, filters.GroupIDs)
	}
	if exclude != "model" {
		conditions, args = appendUsageLogModelWhereConditionsWithAlias(conditions, args, filters.Model, filters.Models, filters.ModelFilterSource, "ul")
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}
