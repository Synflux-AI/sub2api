//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/responseoutcome"
	"github.com/stretchr/testify/require"
)

// 迁移 247 的部分索引谓词是字面量列表，必须与代码中的异常集合（加 unknown）保持一致。
func TestUsageLogFinishReasonIndexPredicateMatchesAbnormalSet(t *testing.T) {
	tx := testTx(t)
	var def string
	require.NoError(t, tx.QueryRowContext(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'usage_logs' AND indexname = $1`,
		usageLogsFinishReasonIndex).Scan(&def))

	expected := append(append([]string{}, responseoutcome.AbnormalReasons...), responseoutcome.ReasonUnknown)
	for _, reason := range expected {
		require.Contains(t, def, "'"+reason+"'", "index predicate must include %s", reason)
	}
	for _, normal := range []string{responseoutcome.ReasonStop, responseoutcome.ReasonToolUse, responseoutcome.ReasonPauseTurn} {
		require.NotContains(t, def, "'"+normal+"'", "normal endings must stay out of the index")
	}
}

// 参数化 IN (...) 在值落入谓词时可命中部分索引（lib/pq 未命名语句按实际参数规划）。
func TestUsageLogFinishReasonFilterUsesPartialIndex(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	_, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off")
	require.NoError(t, err)

	abnormal, err := responseoutcome.ParseFilter("abnormal")
	require.NoError(t, err)
	for _, reasons := range [][]string{{responseoutcome.ReasonRefusal}, abnormal} {
		conditions, args := appendFinishReasonWhereCondition(nil, nil, reasons)
		query := fmt.Sprintf("EXPLAIN SELECT id FROM usage_logs WHERE %s ORDER BY created_at DESC LIMIT 20", strings.Join(conditions, " AND "))
		rows, err := tx.QueryContext(ctx, query, args...)
		require.NoError(t, err)
		var plan strings.Builder
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			plan.WriteString(line + "\n")
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Contains(t, plan.String(), usageLogsFinishReasonIndex, "plan for %v:\n%s", reasons, plan.String())
	}
}
