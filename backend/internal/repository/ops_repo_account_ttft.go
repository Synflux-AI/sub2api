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

// GetAccountModelTTFT 在窗口 [start,end) 内按「账号 × 模型」聚合首 Token 时延分位数,
// 供 AccountTTFTMonitorService 判定账号是否相对同模型的同行明显变慢。
//
// 口径:
//   - 只统计 first_token_ms 非空的行 —— 该字段仅流式请求才记录,
//     非流式与 count_tokens 请求天然被排除,无需额外过滤 is_count_tokens;
//   - 按 model(实际使用的模型)而非 requested_model 分组:基线要比较的是
//     「同一个上游模型下各账号的快慢」,别名/映射前的名字会把同一模型拆成多组、稀释样本;
//   - minSamples 在 SQL 侧过滤,避免把大量长尾小样本行传回进程再丢弃。
//
// 不做平台/分组过滤:usage_logs 无 platform 列,账号与分组是多对多关系;
// 模型本身已经隐含了平台,按模型分组即可得到可比的基线。
func (r *opsRepository) GetAccountModelTTFT(ctx context.Context, start, end time.Time, minSamples int) ([]service.OpsAccountModelTTFTRow, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("ops repository not initialized")
	}
	if minSamples < 1 {
		minSamples = 1
	}

	ctx, cancel := context.WithTimeout(ctx, opsAccountModelTTFTTimeout)
	defer cancel()

	// 时间窗 + account_id 命中 idx_usage_logs_account_created_at;
	// first_token_ms IS NOT NULL 把行数压到只剩流式请求。
	query := `
SELECT
  u.account_id AS account_id,
  COALESCE(a.name, '') AS account_name,
  COALESCE(a.platform, '') AS platform,
  u.model AS model,
  percentile_cont(0.50) WITHIN GROUP (ORDER BY u.first_token_ms) AS ttft_p50_ms,
  percentile_cont(0.95) WITHIN GROUP (ORDER BY u.first_token_ms) AS ttft_p95_ms,
  COUNT(*) AS samples
FROM usage_logs u
LEFT JOIN accounts a ON a.id = u.account_id
WHERE u.created_at >= $1 AND u.created_at < $2
  AND u.first_token_ms IS NOT NULL
  AND u.account_id IS NOT NULL
  AND u.model <> ''
GROUP BY u.account_id, a.name, a.platform, u.model
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
		if err := rows.Scan(&row.AccountID, &name, &plat, &row.Model, &p50, &p95, &row.Samples); err != nil {
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
