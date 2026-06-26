package repository

import (
	"fmt"
	"strings"
)

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
	b.WriteString("SELECT g.k::text AS key, " + labelSelect + " AS label, g.total, g.sla, g.business_limited\n")
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
