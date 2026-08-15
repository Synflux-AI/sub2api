package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// opsAccountModelTTFTTimeout 限制单次 TTFT 聚合耗时。分位数计算比错误率聚合重
// (percentile_cont 需要排序),故比 opsAccountErrorRateTimeout 略宽;巡检周期远大于此,
// 超时丢一轮即可,不阻塞任何请求路径。
const opsAccountModelTTFTTimeout = 15 * time.Second

// GetAccountModelTTFT 在窗口 [start,end) 内按「账号 × 分组 × 实际上游模型」聚合首 Token 时延分位数,
// 供 AccountTTFTMonitorService 判定账号是否相对同调度池的同行明显变慢。
//
// 口径:
//   - 只统计 first_token_ms 非空的行 —— 该字段仅流式请求才记录,
//     非流式与 count_tokens 请求天然被排除,无需额外过滤 is_count_tokens;
//   - 按 group_id + 实际上游模型分组：只有同一调度池中、服务同一上游模型的账号才是可比同行。
//     新数据优先使用 upstream_model，历史数据回退 model。
//   - minSamples 在 SQL 侧过滤,避免把大量长尾小样本行传回进程再丢弃。
func (r *opsRepository) GetAccountModelTTFT(ctx context.Context, start, end time.Time, minSamples int) ([]service.OpsAccountModelTTFTRow, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("ops repository not initialized")
	}
	if minSamples < 1 {
		minSamples = 1
	}

	ctx, cancel := context.WithTimeout(ctx, opsAccountModelTTFTTimeout)
	defer cancel()

	// 时间窗由 usage_logs(created_at) 索引限定；first_token_ms IS NOT NULL
	// 把聚合输入压到流式请求。开关默认关闭，超时时丢弃本轮并 fail-open。
	query := `
SELECT
  u.account_id AS account_id,
  COALESCE(u.group_id, 0) AS group_id,
  COALESCE(a.name, '') AS account_name,
  COALESCE(a.platform, '') AS platform,
  COALESCE(NULLIF(TRIM(u.upstream_model), ''), u.model) AS model,
  percentile_cont(0.50) WITHIN GROUP (ORDER BY u.first_token_ms) AS ttft_p50_ms,
  percentile_cont(0.95) WITHIN GROUP (ORDER BY u.first_token_ms) AS ttft_p95_ms,
  COUNT(*) AS samples
FROM usage_logs u
LEFT JOIN accounts a ON a.id = u.account_id
WHERE u.created_at >= $1 AND u.created_at < $2
  AND u.first_token_ms IS NOT NULL
  AND u.account_id IS NOT NULL
  AND COALESCE(NULLIF(TRIM(u.upstream_model), ''), u.model) <> ''
GROUP BY
  u.account_id,
  COALESCE(u.group_id, 0),
  a.name,
  a.platform,
  COALESCE(NULLIF(TRIM(u.upstream_model), ''), u.model)
HAVING COUNT(*) >= $3`

	rows, err := r.db.QueryContext(ctx, query, start, end, minSamples)
	if err != nil {
		return nil, fmt.Errorf("account model ttft: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []service.OpsAccountModelTTFTRow
	for rows.Next() {
		var row service.OpsAccountModelTTFTRow
		var name, plat sql.NullString
		var p50, p95 sql.NullFloat64
		if err := rows.Scan(&row.AccountID, &row.GroupID, &name, &plat, &row.Model, &p50, &p95, &row.Samples); err != nil {
			return nil, fmt.Errorf("account model ttft scan: %w", err)
		}
		row.AccountName = name.String
		row.Platform = plat.String
		row.TTFTP50Ms = p50.Float64
		row.TTFTP95Ms = p95.Float64
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account model ttft rows: %w", err)
	}
	return out, nil
}
