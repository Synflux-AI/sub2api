package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// opsAlertBreakdownTimeout 限制单次明细聚合耗时,避免告警评估被慢查询拖住。
const opsAlertBreakdownTimeout = 3 * time.Second

// GetAlertErrorBreakdown 在 [start,end) 窗口内回查 ops_error_logs,聚合业务维度明细:
// 窗口请求总数 / 4xx-5xx 拆分 / 平台分布 / Top 用户(含各自错误构成) / Top 错误类型 /
// Top 上游(平台·渠道名·模型) / 样例报错。错误口径由 metricType 决定:upstream_error_rate
// 对齐上游错误率分子(error_owner='provider',含被重试救回的恢复行),其余按 SLA 口径
// (status_code>=400 且非业务限流)。scope 过滤(platform/group)复用 buildErrorWhere /
// buildUsageWhere,保证与触发该告警的指标范围一致。
func (r *opsRepository) GetAlertErrorBreakdown(ctx context.Context, filter *service.OpsDashboardFilter, start, end time.Time, topN int, metricType string) (*service.OpsAlertBreakdown, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("ops repository not initialized")
	}
	if topN <= 0 {
		topN = 5
	}
	if topN > 20 {
		topN = 20
	}

	ctx, cancel := context.WithTimeout(ctx, opsAlertBreakdownTimeout)
	defer cancel()

	base, args, _ := buildErrorWhere(filter, start, end, 1)
	pred := alertBreakdownErrorPredicate(metricType, "")
	where := base + " AND " + pred // 供 TOP/样例 等子查询复用(按 metricType 口径)

	bd := &service.OpsAlertBreakdown{MetricType: metricType}

	// 1) 总错误数 + 4xx/5xx 拆分 + SLA 错误数(分母用)。基线 WHERE 只含时间/scope,口径用 FILTER 表达。
	statusExpr := alertBreakdownStatusExpr(metricType, "")
	slaPred := alertBreakdownSLAPredicate("")
	var slaErrors int64
	totalQ := fmt.Sprintf(`SELECT
		COUNT(*) FILTER (WHERE %[1]s),
		COUNT(*) FILTER (WHERE %[1]s AND %[2]s BETWEEN 400 AND 499),
		COUNT(*) FILTER (WHERE %[1]s AND %[2]s >= 500),
		COUNT(*) FILTER (WHERE %[3]s)
	FROM ops_error_logs %[4]s`, pred, statusExpr, slaPred, base)
	if err := r.db.QueryRowContext(ctx, totalQ, args...).Scan(&bd.TotalErrors, &bd.Client4xx, &bd.Server5xx, &slaErrors); err != nil {
		return nil, fmt.Errorf("alert breakdown total: %w", err)
	}
	bd.OtherErrors = bd.TotalErrors - bd.Client4xx - bd.Server5xx
	if bd.OtherErrors < 0 {
		bd.OtherErrors = 0
	}

	// 2) 窗口请求总数 = 成功(usage_logs) + SLA 错误,与指标分母 requestCountSLA 完全一致。
	//    严禁用 TotalErrors:upstream 口径下被救回的 200 请求已计入 success,会双计。
	if success, _, err := r.queryUsageCounts(ctx, filter, start, end); err == nil {
		bd.WindowRequests = success + slaErrors
	} else {
		bd.WindowRequests = slaErrors
	}

	// 3) 平台分布
	platQ := "SELECT COALESCE(platform,'') AS pf, COUNT(*) AS c FROM ops_error_logs " + where +
		" GROUP BY pf ORDER BY c DESC LIMIT " + fmt.Sprintf("%d", topN)
	if rows, err := r.db.QueryContext(ctx, platQ, args...); err == nil {
		for rows.Next() {
			var p service.OpsAlertPlatformStat
			if err := rows.Scan(&p.Platform, &p.Count); err == nil {
				bd.Platforms = append(bd.Platforms, p)
			}
		}
		_ = rows.Close()
	}

	// 4) Top 用户(仅聚合 user_id;email/notes 与各自错误构成随后回查)
	userQ := "SELECT user_id, COUNT(*) AS c FROM ops_error_logs " + where +
		" AND user_id IS NOT NULL GROUP BY user_id ORDER BY c DESC LIMIT " + fmt.Sprintf("%d", topN)
	userIDs := make([]int64, 0, topN)
	if rows, err := r.db.QueryContext(ctx, userQ, args...); err == nil {
		for rows.Next() {
			var u service.OpsAlertUserStat
			if err := rows.Scan(&u.UserID, &u.Count); err == nil {
				bd.TopUsers = append(bd.TopUsers, u)
				userIDs = append(userIDs, u.UserID)
			}
		}
		_ = rows.Close()
	}
	r.fillAlertUserIdentities(ctx, bd.TopUsers, userIDs)
	r.fillAlertUserErrorComposition(ctx, bd.TopUsers, where, args)

	// 5) Top 错误类型(error_type + 网关状态码 + 上游状态码)
	errQ := "SELECT COALESCE(error_type,'') AS et, COALESCE(status_code,0) AS sc, COALESCE(upstream_status_code,0) AS usc, COUNT(*) AS c FROM ops_error_logs " +
		where + " GROUP BY et, sc, usc ORDER BY c DESC LIMIT " + fmt.Sprintf("%d", topN)
	if rows, err := r.db.QueryContext(ctx, errQ, args...); err == nil {
		for rows.Next() {
			var e service.OpsAlertErrorTypeStat
			if err := rows.Scan(&e.ErrorType, &e.StatusCode, &e.UpstreamStatusCode, &e.Count); err == nil {
				bd.TopErrorTypes = append(bd.TopErrorTypes, e)
			}
		}
		_ = rows.Close()
	}

	// 6) Top 上游(平台 · 渠道名 · 模型);account_id 为空表示未走到选号(客户端错误)。
	// 与 accounts JOIN 时 created_at 列名歧义,改用带别名 e 的等价 where。
	upBase, upArgs, _ := buildErrorWhereAliased(filter, start, end, 1, "e")
	upWhere := upBase + " AND " + alertBreakdownErrorPredicate(metricType, "e.")
	upQ := `SELECT COALESCE(e.account_id,0) AS aid, COALESCE(a.name,'') AS aname, COALESCE(e.platform,'') AS pf, COALESCE(e.model,'') AS md, COUNT(*) AS c
		FROM ops_error_logs e LEFT JOIN accounts a ON a.id = e.account_id ` + upWhere +
		" GROUP BY aid, aname, pf, md ORDER BY c DESC LIMIT " + fmt.Sprintf("%d", topN)
	if rows, err := r.db.QueryContext(ctx, upQ, upArgs...); err == nil {
		for rows.Next() {
			var up service.OpsAlertUpstreamStat
			if err := rows.Scan(&up.AccountID, &up.AccountName, &up.Platform, &up.Model, &up.Count); err == nil {
				bd.TopUpstreams = append(bd.TopUpstreams, up)
			}
		}
		_ = rows.Close()
	}

	// 7) 样例报错(最近 3 条非空报错原文,优先取上游原文)
	sampleQ := "SELECT COALESCE(status_code,0), COALESCE(NULLIF(upstream_error_message,''), error_message, '') AS msg FROM ops_error_logs " + where +
		" AND COALESCE(NULLIF(upstream_error_message,''), error_message, '') <> '' ORDER BY created_at DESC LIMIT 3"
	if rows, err := r.db.QueryContext(ctx, sampleQ, args...); err == nil {
		for rows.Next() {
			var s service.OpsAlertSampleStat
			if err := rows.Scan(&s.StatusCode, &s.Message); err == nil {
				bd.Samples = append(bd.Samples, s)
			}
		}
		_ = rows.Close()
	}

	return bd, nil
}

// fillAlertUserIdentities 批量回查 users 表,把 email/notes 填回 Top 用户列表(按 user_id 匹配)。
func (r *opsRepository) fillAlertUserIdentities(ctx context.Context, stats []service.OpsAlertUserStat, userIDs []int64) {
	if len(stats) == 0 || len(userIDs) == 0 {
		return
	}
	placeholders := make([]string, 0, len(userIDs))
	args := make([]any, 0, len(userIDs))
	for i, id := range userIDs {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, id)
	}
	q := "SELECT id, email, COALESCE(notes,'') FROM users WHERE id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	type ident struct {
		email string
		notes string
	}
	idents := make(map[int64]ident, len(userIDs))
	for rows.Next() {
		var id int64
		var email, notes sql.NullString
		if err := rows.Scan(&id, &email, &notes); err != nil {
			continue
		}
		idents[id] = ident{email: email.String, notes: notes.String}
	}
	for i := range stats {
		if v, ok := idents[stats[i].UserID]; ok {
			stats[i].Email = v.email
			stats[i].Notes = v.notes
		}
	}
}

// fillAlertUserErrorComposition 为 Top 用户回查各自的错误构成(每人 Top 3 类 error_type+status_code)。
func (r *opsRepository) fillAlertUserErrorComposition(ctx context.Context, stats []service.OpsAlertUserStat, where string, args []any) {
	if len(stats) == 0 {
		return
	}
	idArgs := make([]string, 0, len(stats))
	allArgs := make([]any, len(args), len(args)+len(stats))
	copy(allArgs, args)
	for i := range stats {
		allArgs = append(allArgs, stats[i].UserID)
		idArgs = append(idArgs, fmt.Sprintf("$%d", len(args)+i+1))
	}
	// 用窗口序号函数取每个用户的 Top 3 错误构成。
	q := `SELECT user_id, error_type, status_code, c FROM (
		SELECT user_id, COALESCE(error_type,'') AS error_type, COALESCE(status_code,0) AS status_code, COUNT(*) AS c,
		       ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY COUNT(*) DESC) AS rn
		FROM ops_error_logs ` + where + " AND user_id IN (" + strings.Join(idArgs, ",") + `)
		GROUP BY user_id, COALESCE(error_type,''), COALESCE(status_code,0)
	) t WHERE rn <= 3 ORDER BY user_id, c DESC`
	rows, err := r.db.QueryContext(ctx, q, allArgs...)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	byUser := make(map[int64][]service.OpsAlertErrorTypeStat, len(stats))
	for rows.Next() {
		var uid int64
		var e service.OpsAlertErrorTypeStat
		if err := rows.Scan(&uid, &e.ErrorType, &e.StatusCode, &e.Count); err != nil {
			continue
		}
		byUser[uid] = append(byUser[uid], e)
	}
	for i := range stats {
		stats[i].Errors = byUser[stats[i].UserID]
	}
}

// buildErrorWhereAliased 与 buildErrorWhere 等价,但所有列名带表别名前缀(用于 JOIN 场景避免歧义)。
func buildErrorWhereAliased(filter *service.OpsDashboardFilter, start, end time.Time, startIndex int, alias string) (where string, args []any, nextIndex int) {
	platform := ""
	groupID := (*int64)(nil)
	if filter != nil {
		platform = strings.TrimSpace(strings.ToLower(filter.Platform))
		groupID = filter.GroupID
	}
	a := alias + "."
	idx := startIndex
	clauses := make([]string, 0, 5)
	args = make([]any, 0, 5)

	args = append(args, start)
	clauses = append(clauses, fmt.Sprintf("%screated_at >= $%d", a, idx))
	idx++
	args = append(args, end)
	clauses = append(clauses, fmt.Sprintf("%screated_at < $%d", a, idx))
	idx++

	clauses = append(clauses, a+"is_count_tokens = FALSE")

	if groupID != nil && *groupID > 0 {
		args = append(args, *groupID)
		clauses = append(clauses, fmt.Sprintf("%sgroup_id = $%d", a, idx))
		idx++
	}
	if platform != "" {
		args = append(args, platform)
		clauses = append(clauses, fmt.Sprintf("%splatform = $%d", a, idx))
		idx++
	}

	where = "WHERE " + strings.Join(clauses, " AND ")
	return where, args, idx
}

// alertBreakdownErrorPredicate 返回指定指标口径的错误过滤谓词(不含前导 AND)。
// prefix 为 "" 或 "e."(JOIN 别名场景)。upstream 口径逐字对齐 queryErrorCounts 的 upstreamExcl。
func alertBreakdownErrorPredicate(metricType, prefix string) string {
	p := prefix
	if strings.EqualFold(strings.TrimSpace(metricType), "upstream_error_rate") {
		return fmt.Sprintf("%serror_owner='provider' AND NOT %sis_business_limited AND COALESCE(%supstream_status_code,%sstatus_code,0) NOT IN (429,529)", p, p, p, p)
	}
	return fmt.Sprintf("COALESCE(%sstatus_code,0)>=400 AND COALESCE(%sis_business_limited,false)=false", p, p)
}

// alertBreakdownStatusExpr 返回 4xx/5xx 归类用的状态码表达式。
// upstream 口径基于 COALESCE(upstream_status_code,status_code,0)(恢复行 status=200 不会落入 4xx/5xx),其余基于 status_code。
func alertBreakdownStatusExpr(metricType, prefix string) string {
	p := prefix
	if strings.EqualFold(strings.TrimSpace(metricType), "upstream_error_rate") {
		return fmt.Sprintf("COALESCE(%supstream_status_code,%sstatus_code,0)", p, p)
	}
	return fmt.Sprintf("COALESCE(%sstatus_code,0)", p)
}

// alertBreakdownSLAPredicate 返回 SLA 错误谓词(分母用,口径无关),逐字对齐指标 errorCountSLA。
func alertBreakdownSLAPredicate(prefix string) string {
	p := prefix
	return fmt.Sprintf("COALESCE(%sstatus_code,0)>=400 AND NOT %sis_business_limited", p, p)
}
