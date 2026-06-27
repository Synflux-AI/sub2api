package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const opsErrorBreakdownMaxLimit = 100

func (r *opsRepository) GetErrorBreakdown(ctx context.Context, filter *service.OpsDashboardFilter, dimension string, limit int) (*service.OpsErrorBreakdownResponse, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if filter == nil {
		return nil, fmt.Errorf("nil filter")
	}
	if filter.StartTime.IsZero() || filter.EndTime.IsZero() {
		return nil, fmt.Errorf("start_time/end_time required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > opsErrorBreakdownMaxLimit {
		limit = opsErrorBreakdownMaxLimit
	}

	start := filter.StartTime.UTC()
	end := filter.EndTime.UTC()
	where, args, nextIdx := buildErrorWhere(filter, start, end, 1)

	itemsSQL, totalsSQL, err := buildErrorBreakdownQuery(dimension, where, nextIdx)
	if err != nil {
		return nil, err
	}

	resp := &service.OpsErrorBreakdownResponse{Dimension: dimension, Items: []*service.OpsErrorBreakdownItem{}}

	// grand totals（同 where/args，不加 limit），不受 items 的 LIMIT 影响。
	if err := r.db.QueryRowContext(ctx, totalsSQL, args...).Scan(&resp.Total, &resp.SLA, &resp.BusinessLimited); err != nil {
		return nil, err
	}

	// items（where args + limit）。
	itemArgs := append(append([]any{}, args...), limit)
	rows, err := r.db.QueryContext(ctx, itemsSQL, itemArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		it := &service.OpsErrorBreakdownItem{}
		if err := rows.Scan(&it.Key, &it.Label, &it.Total, &it.SLA, &it.BusinessLimited); err != nil {
			return nil, err
		}
		resp.Items = append(resp.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return resp, nil
}

// opsBreakdownDim 描述一个错误分组维度。
//
//	keyExpr   : 内层 GROUP BY 的键表达式（基于 ops_error_logs，无别名）
//	joinSQL   : 外层可选 JOIN（按 PK 关联 g.k），空表示无 JOIN（key 即 label）
//	labelExpr : 外层 label 表达式；空表示用 g.k 文本作 label
type opsBreakdownDim struct {
	keyExpr   string
	joinSQL   string
	labelExpr string
}

// opsErrorBreakdownDims 是 dimension 白名单（写死，杜绝把入参拼进 SQL）。
var opsErrorBreakdownDims = map[string]opsBreakdownDim{
	"user":         {keyExpr: "user_id", joinSQL: "LEFT JOIN users u ON u.id = g.k", labelExpr: "u.email"},
	"account":      {keyExpr: "account_id", joinSQL: "LEFT JOIN accounts a ON a.id = g.k", labelExpr: "a.name"},
	"api_key":      {keyExpr: "api_key_id", joinSQL: "LEFT JOIN api_keys ak ON ak.id = g.k", labelExpr: "ak.name"},
	"group":        {keyExpr: "group_id", joinSQL: "LEFT JOIN groups grp ON grp.id = g.k", labelExpr: "grp.name"},
	"model":        {keyExpr: "COALESCE(requested_model, model, '')"},
	"status_code":  {keyExpr: "COALESCE(upstream_status_code, status_code, 0)"},
	"error_type":   {keyExpr: "error_type"},
	"error_owner":  {keyExpr: "error_owner"},
	"error_phase":  {keyExpr: "error_phase"},
	"error_source": {keyExpr: "error_source"},
	"platform":     {keyExpr: "platform"},
	"severity":     {keyExpr: "severity"},
}

// buildErrorBreakdownQuery 组装 breakdown 的两条 SQL：
//
//	itemsSQL  : 派生表分组 + 外层 JOIN label，ORDER BY total DESC LIMIT $limitArgIndex
//	totalsSQL : 同 where 的全量聚合（grand total，不受 LIMIT 影响）
//
// where 必须是 buildErrorWhere 产出的「无别名」WHERE 串；两条 SQL 共用同一组 args。
func buildErrorBreakdownQuery(dimension, where string, limitArgIndex int) (itemsSQL, totalsSQL string, err error) {
	dim, ok := opsErrorBreakdownDims[dimension]
	if !ok {
		return "", "", fmt.Errorf("unknown dimension: %q", dimension)
	}

	labelSelect := "COALESCE(g.k::text, '')"
	if dim.labelExpr != "" {
		labelSelect = "COALESCE(" + dim.labelExpr + ", g.k::text, '')"
	}

	var b strings.Builder
	b.WriteString("WITH g AS (\n")
	b.WriteString("  SELECT " + dim.keyExpr + " AS k,\n")
	b.WriteString("         COUNT(*) AS total,\n")
	b.WriteString("         COUNT(*) FILTER (WHERE NOT is_business_limited) AS sla,\n")
	b.WriteString("         COUNT(*) FILTER (WHERE is_business_limited) AS business_limited\n")
	b.WriteString("  FROM ops_error_logs\n  " + where + "\n")
	b.WriteString("    AND COALESCE(status_code, 0) >= 400\n")
	b.WriteString("  GROUP BY 1\n)\n")
	// key 必须 COALESCE：user_id/account_id/platform 等可空列分组会产生 NULL 键，
	// 而 NULL 扫描进 Go string 会报错。NULL 键统一落为空串。
	b.WriteString("SELECT COALESCE(g.k::text, '') AS key, " + labelSelect + " AS label, g.total, g.sla, g.business_limited\n")
	b.WriteString("FROM g " + dim.joinSQL + "\n")
	b.WriteString("ORDER BY g.total DESC\n")
	b.WriteString(fmt.Sprintf("LIMIT $%d", limitArgIndex))
	itemsSQL = b.String()

	totalsSQL = "SELECT COUNT(*) AS total,\n" +
		"       COUNT(*) FILTER (WHERE NOT is_business_limited) AS sla,\n" +
		"       COUNT(*) FILTER (WHERE is_business_limited) AS business_limited\n" +
		"FROM ops_error_logs\n" + where + "\n  AND COALESCE(status_code, 0) >= 400"

	return itemsSQL, totalsSQL, nil
}
