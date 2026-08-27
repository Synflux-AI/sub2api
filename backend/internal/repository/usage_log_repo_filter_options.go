package repository

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetUsageFilterOptions returns dimension values present in the current window
// and filter intersection. Model values include both usage and client-error logs.
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
			_ = rows.Close()
			return nil, err
		}
		result.Users = append(result.Users, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	where, args = usageFilterOptionsWhere(startTime, endTime, filters, "model")
	modelExpr := resolveModelDimensionExpressionWithAlias(usagestats.ModelSourceRequested, "ul")
	errorWhere, _ := usageFilterOptionsWhereWithAlias(startTime, endTime, filters, "model", "e", true)
	errorModelExpr := resolveModelDimensionExpressionWithAlias(usagestats.ModelSourceRequested, "e")
	modelQuery := "SELECT DISTINCT " + modelExpr + " AS model FROM usage_logs ul" + where + " AND " + modelExpr + " <> ''" +
		" UNION SELECT DISTINCT " + errorModelExpr + " AS model FROM ops_error_logs e" + errorWhere + " AND " + errorModelExpr + " <> '' ORDER BY 1"
	rows, err = r.sql.QueryContext(ctx, modelQuery, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			_ = rows.Close()
			return nil, err
		}
		result.Models = append(result.Models, model)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
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
			_ = rows.Close()
			return nil, err
		}
		result.Groups = append(result.Groups, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
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
			_ = rows.Close()
			return nil, err
		}
		result.Accounts = append(result.Accounts, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}

func usageFilterOptionsWhere(startTime, endTime time.Time, filters UsageLogFilters, exclude string) (string, []any) {
	return usageFilterOptionsWhereWithAlias(startTime, endTime, filters, exclude, "ul", false)
}

func usageFilterOptionsWhereWithAlias(startTime, endTime time.Time, filters UsageLogFilters, exclude, alias string, errorRows bool) (string, []any) {
	column := func(name string) string { return alias + "." + name }
	conditions := []string{column("created_at") + " >= $1", column("created_at") + " < $2"}
	args := []any{startTime, endTime}
	if errorRows {
		conditions = append(conditions, "(COALESCE("+column("status_code")+", 0) >= 400 OR "+column("error_type")+" = 'cyber_policy')")
	}
	if exclude != "user" {
		conditions, args = appendUsageLogIDWhereCondition(conditions, args, column("user_id"), filters.UserID, filters.UserIDs)
	}
	conditions, args = appendUsageLogIDWhereCondition(conditions, args, column("api_key_id"), filters.APIKeyID, filters.APIKeyIDs)
	if exclude != "account" {
		conditions, args = appendUsageLogIDWhereCondition(conditions, args, column("account_id"), filters.AccountID, filters.AccountIDs)
	}
	if exclude != "group" {
		conditions, args = appendUsageLogIDWhereCondition(conditions, args, column("group_id"), filters.GroupID, filters.GroupIDs)
	}
	if exclude != "model" {
		conditions, args = appendUsageLogModelWhereConditionsWithAlias(conditions, args, filters.Model, filters.Models, filters.ModelFilterSource, alias)
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}
