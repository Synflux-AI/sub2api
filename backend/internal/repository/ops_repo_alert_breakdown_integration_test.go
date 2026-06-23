//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 真实 Postgres 验证 Top 上游「逐次失败尝试」聚合:展开 upstream_errors JSONB 数组,
// 按 event.account_id 计数,排除 429/529,JOIN accounts 解析渠道名,不带 model 维度。
// 关键回归:一个请求里失败过 A、B,A、B 各计一次(请求级只会归到最后失败的 B);
// 同一渠道的 429 尝试不计入(对齐错误率分子口径)。
func TestGetAlertErrorBreakdown_UpstreamTopPerAttempt_Integration(t *testing.T) {
	ctx := context.Background()
	repo := &opsRepository{db: integrationDB}

	const grp = int64(987654)
	const accA, accB = int64(900001), int64(900002)

	cleanup := func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE group_id=$1`, grp)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id IN ($1,$2)`, accA, accB)
	}
	cleanup()
	t.Cleanup(cleanup)

	_, err := integrationDB.ExecContext(ctx,
		`INSERT INTO accounts (id,name,platform,type) VALUES ($1,'chan-A','anthropic','apikey'),($2,'chan-B','anthropic','apikey')`,
		accA, accB)
	require.NoError(t, err)

	now := time.Now().UTC()
	start := now.Add(-10 * time.Minute)
	end := now.Add(10 * time.Minute)

	// 一行被救回(status=200, error_owner=provider),含三次失败尝试:A/500、B/500、B/429。
	// 期望逐次聚合:A×1、B×1(B 的 429 被排除,不会变成 ×2)。
	events := `[
		{"account_id":900001,"platform":"anthropic","upstream_status_code":500},
		{"account_id":900002,"platform":"anthropic","upstream_status_code":500},
		{"account_id":900002,"platform":"anthropic","upstream_status_code":429}
	]`
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO ops_error_logs
			(group_id, platform, error_phase, error_type, status_code, upstream_status_code,
			 error_owner, is_business_limited, is_count_tokens, upstream_errors, created_at)
		VALUES ($1,'anthropic','upstream','upstream_error',200,500,'provider',false,false,$2::jsonb,$3)`,
		grp, events, now)
	require.NoError(t, err)

	g := grp
	filter := &service.OpsDashboardFilter{GroupID: &g}
	bd, err := repo.GetAlertErrorBreakdown(ctx, filter, start, end, 5, "upstream_error_rate")
	require.NoError(t, err)

	got := map[int64]int64{}
	name := map[int64]string{}
	for _, up := range bd.TopUpstreams {
		got[up.AccountID] = up.Count
		name[up.AccountID] = up.AccountName
		require.Empty(t, up.Model, "逐次口径无 model 维度")
	}
	require.Equal(t, int64(1), got[accA], "A 失败一次,计一次")
	require.Equal(t, int64(1), got[accB], "B 失败:500 计一次,429 被排除(不计两次)")
	require.Equal(t, "chan-A", name[accA], "JOIN accounts 解析渠道名")
	require.Equal(t, "chan-B", name[accB], "JOIN accounts 解析渠道名")
}
