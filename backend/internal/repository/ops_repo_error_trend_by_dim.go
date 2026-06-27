package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const opsErrorTrendByDimMaxLimit = 50

// buildErrorTrendByDimQuery 组装单条 CTE：
//
//	topn   : 窗口内按维度键的 Top-N（仅取键，ORDER BY 计数）
//	labeled: 给 topn 键拼 label（走维度 joinSQL）
//	agg    : 逐时间桶 × (Top-N 键 | '__others__') 的三口径计数
//
// where 必须是 buildErrorWhere 产出的「无别名」串；其占位符在 topn/agg 两处复用
// （Postgres 允许同一占位符多次出现，故 args 只传一份）。
// $limitArgIndex 是 Top-N 的 LIMIT 占位符。
func buildErrorTrendByDimQuery(dimension, where, bucketExpr string, limitArgIndex int) (string, error) {
	dim, ok := opsErrorBreakdownDims[dimension]
	if !ok {
		return "", fmt.Errorf("unknown dimension: %q", dimension)
	}
	labelSelect := "g.k::text"
	if dim.labelExpr != "" {
		labelSelect = "COALESCE(" + dim.labelExpr + ", g.k::text)"
	}
	errFilter := "COALESCE(status_code, 0) >= 400"

	var b strings.Builder
	_, _ = b.WriteString("WITH topn AS (\n")
	_, _ = b.WriteString("  SELECT " + dim.keyExpr + " AS k, COUNT(*) AS c\n")
	_, _ = b.WriteString("  FROM ops_error_logs\n  " + where + "\n    AND " + errFilter + "\n")
	_, _ = b.WriteString("  GROUP BY 1 ORDER BY c DESC, 1 ASC\n")
	_, _ = b.WriteString(fmt.Sprintf("  LIMIT $%d\n", limitArgIndex))
	_, _ = b.WriteString("),\n")
	_, _ = b.WriteString("labeled AS (\n")
	_, _ = b.WriteString("  SELECT COALESCE(g.k::text, '') AS key, " + labelSelect + " AS label\n")
	_, _ = b.WriteString("  FROM topn g " + dim.joinSQL + "\n")
	_, _ = b.WriteString("),\n")
	_, _ = b.WriteString("agg AS (\n")
	_, _ = b.WriteString("  SELECT " + bucketExpr + " AS bucket,\n")
	// NULL 键(如未鉴权请求 user_id IS NULL)的归类:用 IS NOT DISTINCT FROM 让 NULL=NULL 成立,
	// 否则 `NULL IN (SELECT k FROM topn)` 恒为 NULL→落入 ELSE,即便 NULL 组本身是 Top-N 也会被吞进其它。
	_, _ = b.WriteString("         CASE WHEN EXISTS (SELECT 1 FROM topn WHERE k IS NOT DISTINCT FROM " + dim.keyExpr + ") THEN COALESCE(" + dim.keyExpr + "::text, '') ELSE '__others__' END AS key,\n")
	_, _ = b.WriteString("         COUNT(*) FILTER (WHERE " + errFilter + ") AS total,\n")
	_, _ = b.WriteString("         COUNT(*) FILTER (WHERE " + errFilter + " AND NOT is_business_limited) AS sla,\n")
	_, _ = b.WriteString("         COUNT(*) FILTER (WHERE " + errFilter + " AND is_business_limited) AS business_limited\n")
	_, _ = b.WriteString("  FROM ops_error_logs\n  " + where + "\n    AND " + errFilter + "\n")
	_, _ = b.WriteString("  GROUP BY 1, 2\n")
	_, _ = b.WriteString(")\n")
	_, _ = b.WriteString("SELECT a.bucket, a.key,\n")
	_, _ = b.WriteString("       COALESCE(l.label, CASE WHEN a.key = '__others__' THEN '其它' ELSE a.key END) AS label,\n")
	_, _ = b.WriteString("       a.total, a.sla, a.business_limited\n")
	_, _ = b.WriteString("FROM agg a LEFT JOIN labeled l ON l.key = a.key\n")
	_, _ = b.WriteString("ORDER BY a.bucket ASC, a.total DESC")
	return b.String(), nil
}

// GetErrorTrendByDim 返回某维度下「逐时间桶 × Top-N 键(+其它)」的错误计数长表。
func (r *opsRepository) GetErrorTrendByDim(ctx context.Context, filter *service.OpsDashboardFilter, dimension string, bucketSeconds, limit int) (*service.OpsErrorTrendByDimResponse, error) {
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
		limit = 8
	}
	if limit > opsErrorTrendByDimMaxLimit {
		limit = opsErrorTrendByDimMaxLimit
	}
	if bucketSeconds != 60 && bucketSeconds != 300 && bucketSeconds != 3600 {
		bucketSeconds = 60
	}

	start := filter.StartTime.UTC()
	end := filter.EndTime.UTC()
	where, args, nextIdx := buildErrorWhere(filter, start, end, 1)
	bucketExpr := opsBucketExprForError(bucketSeconds)

	q, err := buildErrorTrendByDimQuery(dimension, where, bucketExpr, nextIdx)
	if err != nil {
		return nil, err
	}
	queryArgs := append(append([]any{}, args...), limit)

	rows, err := r.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	resp := &service.OpsErrorTrendByDimResponse{
		Dimension: dimension,
		Bucket:    opsBucketLabel(bucketSeconds),
		Points:    []*service.OpsErrorTrendByDimPoint{},
	}
	for rows.Next() {
		var bucket time.Time
		p := &service.OpsErrorTrendByDimPoint{}
		if err := rows.Scan(&bucket, &p.Key, &p.Label, &p.Total, &p.SLA, &p.BusinessLimited); err != nil {
			return nil, err
		}
		p.BucketStart = bucket.UTC()
		resp.Points = append(resp.Points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return resp, nil
}
