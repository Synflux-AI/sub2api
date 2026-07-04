//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func sumRequestTotals(pts []*service.OpsRequestTotalPoint) int64 {
	var s int64
	for _, p := range pts {
		s += p.RequestTotal
	}
	return s
}

// TestGetErrorTrendByDim_RequestTotals_Invariants_Integration 用真实 Postgres 锁定错误率虚线分母口径
// （见 memory error-rate-line-and-ops-where-pitfall）：
//   - request_total = 成功 usage_logs + 错误 ops_error_logs(status>=400)
//   - 只随实体筛选(user/model)变，不随 error_owner 变
//   - status<400 的错误不计入分母
func TestGetErrorTrendByDim_RequestTotals_Invariants_Integration(t *testing.T) {
	ctx := context.Background()
	repo := &opsRepository{db: integrationDB}

	u1 := mustCreateUser(t, integrationEntClient, &service.User{Email: "rt-u1@example.com", Username: "rt-u1"})
	u2 := mustCreateUser(t, integrationEntClient, &service.User{Email: "rt-u2@example.com", Username: "rt-u2"})
	a1 := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "rt-a1"})
	a2 := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "rt-a2"})
	k1 := mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: u1.ID})
	k2 := mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: u2.ID})

	now := time.Now().UTC()
	start := now.Add(-20 * time.Minute)
	end := now.Add(20 * time.Minute)
	at := now.Add(-1 * time.Minute)

	cleanup := func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE user_id IN ($1,$2)`, u1.ID, u2.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM usage_logs WHERE user_id IN ($1,$2)`, u1.ID, u2.ID)
	}
	cleanup()
	t.Cleanup(cleanup)

	insertUsage := func(userID, apiKeyID, accountID int64, model string) {
		_, err := integrationDB.ExecContext(ctx,
			`INSERT INTO usage_logs (user_id, api_key_id, account_id, model, input_tokens, output_tokens, total_cost, actual_cost, created_at)
			 VALUES ($1,$2,$3,$4,1,1,0.01,0.01,$5)`,
			userID, apiKeyID, accountID, model, at)
		require.NoError(t, err)
	}
	insertErr := func(userID, accountID, apiKeyID int64, model string, status int, owner string, businessLimited bool) {
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO ops_error_logs
				(user_id, account_id, api_key_id, model, platform, error_phase, error_type,
				 status_code, error_owner, is_business_limited, is_count_tokens, created_at)
			VALUES ($1,$2,$3,$4,'anthropic','upstream','upstream_error',$5,$6,$7,false,$8)`,
			userID, accountID, apiKeyID, model, status, owner, businessLimited, at)
		require.NoError(t, err)
	}

	// 成功 usage：U1/m-one ×3、U2/m-two ×2
	for i := 0; i < 3; i++ {
		insertUsage(u1.ID, k1.ID, a1.ID, "m-one")
	}
	for i := 0; i < 2; i++ {
		insertUsage(u2.ID, k2.ID, a2.ID, "m-two")
	}
	// 错误 status>=400（计入分母）：U1 500×2、U1 429(业务限流)×1、U2 500×1
	insertErr(u1.ID, a1.ID, k1.ID, "m-one", 500, "provider", false)
	insertErr(u1.ID, a1.ID, k1.ID, "m-one", 500, "provider", false)
	insertErr(u1.ID, a1.ID, k1.ID, "m-one", 429, "user", true)
	insertErr(u2.ID, a2.ID, k2.ID, "m-two", 500, "provider", false)
	// status<400 不计入分母
	insertErr(u1.ID, a1.ID, k1.ID, "m-one", 200, "provider", false)

	sumFor := func(f *service.OpsDashboardFilter) int64 {
		resp, err := repo.GetErrorTrendByDim(ctx, f, "user", 3600, 8)
		require.NoError(t, err)
		return sumRequestTotals(resp.RequestTotals)
	}

	base := sumFor(&service.OpsDashboardFilter{StartTime: start, EndTime: end})
	require.Equal(t, int64(9), base, "5 usage + 4 err(status>=400)；status<400 排除")

	owner := sumFor(&service.OpsDashboardFilter{StartTime: start, EndTime: end, ErrorOwner: "provider"})
	require.Equal(t, int64(9), owner, "分母不随 error_owner 变")

	byUser := sumFor(&service.OpsDashboardFilter{StartTime: start, EndTime: end, UserID: &u1.ID})
	require.Equal(t, int64(6), byUser, "U1：3 usage + 3 err")

	byModel := sumFor(&service.OpsDashboardFilter{StartTime: start, EndTime: end, Model: "m-two"})
	require.Equal(t, int64(3), byModel, "m-two：2 usage + 1 err")
}
