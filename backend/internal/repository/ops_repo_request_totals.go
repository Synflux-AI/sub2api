package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// buildErrorEntityWhere 构造 ops_error_logs 的「仅实体过滤」where，用于错误率虚线分母的错误侧。
//
// 与 buildErrorWhere 的区别：剥离 error_owner/error_source/error_type/error_phase/severity/status_codes
// 这些「随 view 变」的维度，只保留 时间窗 + is_count_tokens=FALSE + group/platform + 实体(user/account/key/model)。
// 「分母只随实体筛选变，不随 error_owner/error_type 变」由此保证。
// status>=400 由调用方按需追加（与 buildErrorWhere 保持一致，不在此写死）。
func buildErrorEntityWhere(filter *service.OpsDashboardFilter, start, end time.Time, startIndex int) (where string, args []any, nextIndex int) {
	platform := ""
	groupID := (*int64)(nil)
	if filter != nil {
		platform = strings.TrimSpace(strings.ToLower(filter.Platform))
		groupID = filter.GroupID
	}

	idx := startIndex
	clauses := make([]string, 0, 8)
	args = make([]any, 0, 8)

	args = append(args, start)
	clauses = append(clauses, fmt.Sprintf("created_at >= $%d", idx))
	idx++
	args = append(args, end)
	clauses = append(clauses, fmt.Sprintf("created_at < $%d", idx))
	idx++

	clauses = append(clauses, "is_count_tokens = FALSE")

	if groupID != nil && *groupID > 0 {
		args = append(args, *groupID)
		clauses = append(clauses, fmt.Sprintf("group_id = $%d", idx))
		idx++
	}
	if platform != "" {
		args = append(args, platform)
		clauses = append(clauses, fmt.Sprintf("platform = $%d", idx))
		idx++
	}
	if filter != nil {
		if filter.UserID != nil && *filter.UserID > 0 {
			args = append(args, *filter.UserID)
			clauses = append(clauses, fmt.Sprintf("user_id = $%d", idx))
			idx++
		}
		if filter.AccountID != nil && *filter.AccountID > 0 {
			args = append(args, *filter.AccountID)
			clauses = append(clauses, fmt.Sprintf("account_id = $%d", idx))
			idx++
		}
		if filter.APIKeyID != nil && *filter.APIKeyID > 0 {
			args = append(args, *filter.APIKeyID)
			clauses = append(clauses, fmt.Sprintf("api_key_id = $%d", idx))
			idx++
		}
		if m := strings.TrimSpace(filter.Model); m != "" {
			args = append(args, m)
			clauses = append(clauses, fmt.Sprintf("COALESCE(requested_model, model, '') = $%d", idx))
			idx++
		}
	}

	where = "WHERE " + strings.Join(clauses, " AND ")
	return where, args, idx
}

// buildUsageEntityWhere 构造 usage_logs 的「仅实体过滤」where，用于错误率虚线分母的成功侧。
//
// 在 buildUsageWhere（只有 time+group+platform）基础上补齐 user/account/key/model 实体过滤（ul. 前缀），
// 使分母成功侧随实体下钻同步缩小，与错误侧口径对齐。否则分母偏大、错误率偏低。
func buildUsageEntityWhere(filter *service.OpsDashboardFilter, start, end time.Time, startIndex int) (join string, where string, args []any, nextIndex int) {
	platform := ""
	groupID := (*int64)(nil)
	if filter != nil {
		platform = strings.TrimSpace(strings.ToLower(filter.Platform))
		groupID = filter.GroupID
	}

	idx := startIndex
	clauses := make([]string, 0, 8)
	args = make([]any, 0, 8)

	args = append(args, start)
	clauses = append(clauses, fmt.Sprintf("ul.created_at >= $%d", idx))
	idx++
	args = append(args, end)
	clauses = append(clauses, fmt.Sprintf("ul.created_at < $%d", idx))
	idx++

	if groupID != nil && *groupID > 0 {
		args = append(args, *groupID)
		clauses = append(clauses, fmt.Sprintf("ul.group_id = $%d", idx))
		idx++
	}
	if platform != "" {
		// 与 buildUsageWhere 一致：优先 group.platform，回退 account.platform，避免漏掉 group_id 为 NULL 的行。
		join = "LEFT JOIN groups g ON g.id = ul.group_id LEFT JOIN accounts a ON a.id = ul.account_id"
		args = append(args, platform)
		clauses = append(clauses, fmt.Sprintf("COALESCE(NULLIF(g.platform,''), a.platform) = $%d", idx))
		idx++
	}
	if filter != nil {
		if filter.UserID != nil && *filter.UserID > 0 {
			args = append(args, *filter.UserID)
			clauses = append(clauses, fmt.Sprintf("ul.user_id = $%d", idx))
			idx++
		}
		if filter.AccountID != nil && *filter.AccountID > 0 {
			args = append(args, *filter.AccountID)
			clauses = append(clauses, fmt.Sprintf("ul.account_id = $%d", idx))
			idx++
		}
		if filter.APIKeyID != nil && *filter.APIKeyID > 0 {
			args = append(args, *filter.APIKeyID)
			clauses = append(clauses, fmt.Sprintf("ul.api_key_id = $%d", idx))
			idx++
		}
		if m := strings.TrimSpace(filter.Model); m != "" {
			args = append(args, m)
			clauses = append(clauses, fmt.Sprintf("COALESCE(ul.requested_model, ul.model, '') = $%d", idx))
			idx++
		}
	}

	where = "WHERE " + strings.Join(clauses, " AND ")
	return join, where, args, idx
}

// buildRequestTotalsQuery 组装错误率虚线分母的逐桶查询：
// 成功侧(usage_logs, usageWhere) + 错误侧(ops_error_logs, errorWhere + status>=400)，UNION ALL 后按桶求和。
// usageWhere/errorWhere 必须由 buildUsageEntityWhere/buildErrorEntityWhere 产出（仅实体过滤），
// 占位符已由调用方分配（usage 在前，error 在后）。
func buildRequestTotalsQuery(usageJoin, usageWhere, errorWhere, usageBucketExpr, errorBucketExpr string) string {
	var b strings.Builder
	_, _ = b.WriteString("WITH usage_buckets AS (\n")
	_, _ = b.WriteString("  SELECT " + usageBucketExpr + " AS bucket, COUNT(*) AS c\n")
	_, _ = b.WriteString("  FROM usage_logs ul\n  " + usageJoin + "\n  " + usageWhere + "\n")
	_, _ = b.WriteString("  GROUP BY 1\n),\n")
	_, _ = b.WriteString("error_buckets AS (\n")
	_, _ = b.WriteString("  SELECT " + errorBucketExpr + " AS bucket, COUNT(*) AS c\n")
	_, _ = b.WriteString("  FROM ops_error_logs\n  " + errorWhere + "\n    AND COALESCE(status_code, 0) >= 400\n")
	_, _ = b.WriteString("  GROUP BY 1\n),\n")
	_, _ = b.WriteString("combined AS (\n")
	_, _ = b.WriteString("  SELECT bucket, SUM(c) AS request_total\n")
	_, _ = b.WriteString("  FROM (\n")
	_, _ = b.WriteString("    SELECT bucket, c FROM usage_buckets\n")
	_, _ = b.WriteString("    UNION ALL\n")
	_, _ = b.WriteString("    SELECT bucket, c FROM error_buckets\n")
	_, _ = b.WriteString("  ) t\n  GROUP BY bucket\n)\n")
	_, _ = b.WriteString("SELECT bucket, request_total FROM combined ORDER BY bucket ASC")
	return b.String()
}

// getRequestTotalsByBucket 返回窗口内逐桶的请求总数分母，供错误率虚线使用。
// 两侧都走「仅实体过滤」where，保证分母不随 error_owner/error_type 等 view 维度变化。
func (r *opsRepository) getRequestTotalsByBucket(ctx context.Context, filter *service.OpsDashboardFilter, bucketSeconds int) ([]*service.OpsRequestTotalPoint, error) {
	start := filter.StartTime.UTC()
	end := filter.EndTime.UTC()

	usageJoin, usageWhere, usageArgs, next := buildUsageEntityWhere(filter, start, end, 1)
	errorWhere, errorArgs, _ := buildErrorEntityWhere(filter, start, end, next)

	q := buildRequestTotalsQuery(
		usageJoin, usageWhere, errorWhere,
		opsBucketExprForUsage(bucketSeconds), opsBucketExprForError(bucketSeconds),
	)
	args := append(append([]any{}, usageArgs...), errorArgs...)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*service.OpsRequestTotalPoint, 0, 256)
	for rows.Next() {
		var bucket time.Time
		var total int64
		if err := rows.Scan(&bucket, &total); err != nil {
			return nil, err
		}
		out = append(out, &service.OpsRequestTotalPoint{BucketStart: bucket.UTC(), RequestTotal: total})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
