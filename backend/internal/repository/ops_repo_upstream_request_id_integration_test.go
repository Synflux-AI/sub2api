//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 错误日志的“上游ID”取 upstream_errors 中最后一次带标识的尝试；
// 空标识的尝试被跳过，非数组/空值的 upstream_errors 得到空串。
func TestOpsErrorLogUpstreamRequestID_Integration(t *testing.T) {
	ctx := context.Background()
	repo := &opsRepository{db: integrationDB}

	const grp = int64(987655)
	cleanup := func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE group_id=$1`, grp)
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	insert := func(events any, createdAt time.Time) int64 {
		var id int64
		err := integrationDB.QueryRowContext(ctx, `
			INSERT INTO ops_error_logs
				(group_id, platform, error_phase, error_type, severity, status_code,
				 error_owner, is_business_limited, is_count_tokens, upstream_errors, created_at)
			VALUES ($1,'anthropic','upstream','upstream_error','error',502,'provider',false,false,$2::jsonb,$3)
			RETURNING id`,
			grp, events, createdAt,
		).Scan(&id)
		require.NoError(t, err)
		return id
	}

	withIDs := insert(`[
		{"account_id":1,"upstream_status_code":500,"upstream_request_id":"up-first"},
		{"account_id":2,"upstream_status_code":502,"upstream_request_id":"up-last"},
		{"account_id":2,"kind":"failover","message":"no id"}
	]`, now)
	noIDs := insert(`[{"account_id":1,"upstream_status_code":500}]`, now.Add(-time.Second))
	nullEvents := insert(nil, now.Add(-2*time.Second))

	g := grp
	list, err := repo.ListErrorLogs(ctx, &service.OpsErrorLogFilter{
		GroupID:   &g,
		StartTime: ptrTime(now.Add(-time.Minute)),
		EndTime:   ptrTime(now.Add(time.Minute)),
		View:      "all",
		Page:      1,
		PageSize:  10,
	})
	require.NoError(t, err)
	got := map[int64]string{}
	for _, item := range list.Errors {
		got[item.ID] = item.UpstreamRequestID
	}
	require.Len(t, got, 3)
	require.Equal(t, "up-last", got[withIDs])
	require.Empty(t, got[noIDs])
	require.Empty(t, got[nullEvents])

	detail, err := repo.GetErrorLogByID(ctx, withIDs)
	require.NoError(t, err)
	require.Equal(t, "up-last", detail.UpstreamRequestID)
}
