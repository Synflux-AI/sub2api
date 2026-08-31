//go:build unit

package repository

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var phaseLatencyColumns = []string{
	"auth_latency_ms",
	"routing_latency_ms",
	"upstream_latency_ms",
	"response_latency_ms",
}

func newPhaseLatencyUsageLog(phase service.PhaseLatency) *service.UsageLog {
	return &service.UsageLog{
		UserID:       1,
		APIKeyID:     2,
		AccountID:    3,
		RequestID:    "req-phase-latency",
		Model:        "claude-3",
		InputTokens:  10,
		OutputTokens: 5,
		TotalCost:    1.0,
		ActualCost:   1.0,
		PhaseLatency: phase,
		CreatedAt:    time.Now().UTC(),
	}
}

// phaseLatencyArgIdx 是 auth_latency_ms 在参数表里的下标。
// INSERT 列表不含 id，duration_ms 下标 33、first_token_ms 下标 34，四段紧随其后。
const phaseLatencyArgIdx = 35

func phaseLatencyArgs(t *testing.T, prepared usageLogInsertPrepared) []sql.NullInt64 {
	t.Helper()
	out := make([]sql.NullInt64, 0, len(phaseLatencyColumns))
	for i := range phaseLatencyColumns {
		arg := prepared.args[phaseLatencyArgIdx+i]
		v, ok := arg.(sql.NullInt64)
		require.True(t, ok, "%s arg should be sql.NullInt64, got %T", phaseLatencyColumns[i], arg)
		out = append(out, v)
	}
	return out
}

// TestPrepareUsageLogInsert_PhaseLatencyArgWiring 把四个阶段耗时钉在参数表的固定槽位上。
// 顺序错位是这类改动最危险的失败模式：SQL 仍然执行成功，只是把「路由」的毫秒数写进了「上游」列。
func TestPrepareUsageLogInsert_PhaseLatencyArgWiring(t *testing.T) {
	require.Len(t, usageLogInsertArgTypes, 67)
	for i := range phaseLatencyColumns {
		require.Equal(t, "integer", usageLogInsertArgTypes[phaseLatencyArgIdx+i],
			"%s arg type must be integer", phaseLatencyColumns[i])
	}

	auth, routing, upstream, response := 7, 19296, 29201, 460
	prepared := prepareUsageLogInsert(newPhaseLatencyUsageLog(service.PhaseLatency{
		AuthLatencyMs:     &auth,
		RoutingLatencyMs:  &routing,
		UpstreamLatencyMs: &upstream,
		ResponseLatencyMs: &response,
	}))
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	args := phaseLatencyArgs(t, prepared)
	for i, want := range []int{auth, routing, upstream, response} {
		require.True(t, args[i].Valid, "%s must be non-NULL", phaseLatencyColumns[i])
		require.Equal(t, int64(want), args[i].Int64,
			"%s landed in the wrong arg slot", phaseLatencyColumns[i])
	}
}

// TestPrepareUsageLogInsert_PhaseLatencyNullWhenAbsent 区分「没测到」与「测到 0ms」：
// 未插桩的平台路径写 NULL，亚毫秒的真实测量写 0。把 nil 补成 0 会让两者永久混淆。
func TestPrepareUsageLogInsert_PhaseLatencyNullWhenAbsent(t *testing.T) {
	absent := phaseLatencyArgs(t, prepareUsageLogInsert(newPhaseLatencyUsageLog(service.PhaseLatency{})))
	for i := range phaseLatencyColumns {
		require.False(t, absent[i].Valid,
			"%s must be NULL when the path is not instrumented", phaseLatencyColumns[i])
	}

	zero := 0
	measured := phaseLatencyArgs(t, prepareUsageLogInsert(newPhaseLatencyUsageLog(service.PhaseLatency{
		AuthLatencyMs:     &zero,
		RoutingLatencyMs:  &zero,
		UpstreamLatencyMs: &zero,
		ResponseLatencyMs: &zero,
	})))
	for i := range phaseLatencyColumns {
		require.True(t, measured[i].Valid,
			"%s measured at 0ms must stay 0, not collapse to NULL", phaseLatencyColumns[i])
		require.Equal(t, int64(0), measured[i].Int64)
	}
}

// TestUsageLogQueries_IncludePhaseLatency 守住四组 INSERT 变体与 SELECT 列表 ——
// 漏掉任何一组，只有走到那条写入路径的请求才会丢列，本地很难复现。
func TestUsageLogQueries_IncludePhaseLatency(t *testing.T) {
	for _, col := range phaseLatencyColumns {
		require.Contains(t, usageLogSelectColumns, col, "SELECT column list must include %s", col)
	}

	log := newPhaseLatencyUsageLog(service.PhaseLatency{})
	prepared := prepareUsageLogInsert(log)
	key := usageLogBatchKey(log.RequestID, log.APIKeyID)

	batchQuery, batchArgs := buildUsageLogBatchInsertQuery([]string{key},
		map[string]usageLogInsertPrepared{key: prepared})
	bestEffortQuery, bestEffortArgs := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})
	for _, col := range phaseLatencyColumns {
		require.GreaterOrEqual(t, strings.Count(batchQuery, col), 3,
			"batch INSERT must reference %s in the column list, the input CTE and the SELECT", col)
		require.Contains(t, bestEffortQuery, col)
	}
	require.Len(t, batchArgs, len(prepared.args)+1)
	require.Len(t, bestEffortArgs, len(prepared.args))
}
