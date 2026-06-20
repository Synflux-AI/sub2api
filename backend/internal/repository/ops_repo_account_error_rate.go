package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// opsAccountErrorRateTimeout 限制单次按账号错误率聚合耗时,避免巡检被慢查询拖住。
// 取 8s 留出冷缓存/启动期争抢的余量(巡检周期 60s,留这点尾延迟无碍)。
const opsAccountErrorRateTimeout = 8 * time.Second

// GetAccountErrorRates 在窗口 [start,end) 内按账号聚合「请求总数」与「上游错误数」,
// 供 AccountErrorRateMonitorService 对每个渠道独立判定上游错误率。
//
// 口径与整体 upstream_error_rate 完全一致:
//   - 分子(上游错误)= ops_error_logs 中 error_owner='provider' 且非业务限流且
//     COALESCE(upstream_status_code,status_code,0) NOT IN (429,529) 的条数;
//   - 分母(请求总数)= 成功请求(usage_logs) + SLA 错误(ops_error_logs 中 status_code>=400
//     且非业务限流);错误侧与整体口径一致地排除 count_tokens(is_count_tokens=FALSE)。
//
// 仅返回窗口内有上游错误(upstream_errors>0)且 account_id>0 的行:错误为 0 的账号错误率必为 0、
// 永远不会破阈值,过滤掉可大幅减小结果集(账号多时尤为重要),不影响判定结果。
// 平台/分组维度暂不过滤(全局单一阈值,巡检所有渠道);账号名/平台取自 accounts 表。
func (r *opsRepository) GetAccountErrorRates(ctx context.Context, start, end time.Time) ([]service.OpsAccountErrorRateRow, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("ops repository not initialized")
	}

	ctx, cancel := context.WithTimeout(ctx, opsAccountErrorRateTimeout)
	defer cancel()

	// 以「错误账号」驱动(error_log 是低频表,窗口内行数远小于 usage_logs):
	//   1) errs: 按 account_id 聚合窗口内 SLA 错误数与上游错误数;
	//   2) bad: 仅保留有上游错误(upstream_err>0)的少数账号 —— 这是最终要返回的全集;
	//   3) success: 只对 bad 里的账号统计成功数(account_id IN (...) 命中
	//      idx_usage_logs_account_created_at,避免无条件全量扫描 usage_logs 时间窗)。
	// 口径与整体 upstream_error_rate 完全一致(requests = 成功 + SLA 错误)。窗口序号 $1=start $2=end。
	// 注意:usage_logs 无 platform 列、accounts 无 group_id 列(账号与分组为多对多),故不做
	// 平台/分组过滤,也不取 group_id。
	query := `
WITH errs AS (
  SELECT account_id,
    COUNT(*) FILTER (WHERE COALESCE(status_code, 0) >= 400 AND NOT is_business_limited) AS sla_err,
    COUNT(*) FILTER (WHERE error_owner = 'provider' AND NOT is_business_limited AND COALESCE(upstream_status_code, status_code, 0) NOT IN (429, 529)) AS upstream_err
  FROM ops_error_logs
  WHERE created_at >= $1 AND created_at < $2 AND is_count_tokens = FALSE AND account_id IS NOT NULL
  GROUP BY account_id
),
bad AS (
  SELECT account_id, sla_err, upstream_err FROM errs WHERE upstream_err > 0
),
success AS (
  SELECT account_id, COUNT(*) AS ok_cnt
  FROM usage_logs
  WHERE created_at >= $1 AND created_at < $2 AND account_id IN (SELECT account_id FROM bad)
  GROUP BY account_id
)
SELECT
  b.account_id AS account_id,
  COALESCE(a.name, '') AS account_name,
  COALESCE(a.platform, '') AS platform,
  COALESCE(s.ok_cnt, 0) + b.sla_err AS requests,
  b.upstream_err AS upstream_errors
FROM bad b
LEFT JOIN success s ON s.account_id = b.account_id
LEFT JOIN accounts a ON a.id = b.account_id`

	rows, err := r.db.QueryContext(ctx, query, start, end)
	if err != nil {
		return nil, fmt.Errorf("account error rates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []service.OpsAccountErrorRateRow
	for rows.Next() {
		var row service.OpsAccountErrorRateRow
		var name, plat sql.NullString
		if err := rows.Scan(&row.AccountID, &name, &plat, &row.Requests, &row.UpstreamErrors); err != nil {
			return nil, fmt.Errorf("account error rates scan: %w", err)
		}
		row.AccountName = name.String
		row.Platform = plat.String
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account error rates rows: %w", err)
	}
	return out, nil
}
