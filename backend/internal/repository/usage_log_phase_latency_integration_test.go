//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestUsageLog_PhaseLatencyPersistence 用真库往返证明四段耗时的列偏移是对的。
//
// 这是纯参数顺序单测覆盖不到的失败模式：写入侧和扫描侧各自「自洽」但相对错位时，
// SQL 照样执行成功，只是把路由的毫秒数读成了上游的 —— 只有插进去再读回来才能发现。
// 所以四个值故意取互不相同的数量级，任何两列互换都会让断言失败。
func TestUsageLog_PhaseLatencyPersistence(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{Email: "phase-latency-" + uuid.NewString() + "@example.com"})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-phase-" + uuid.NewString(), Name: "k"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-phase-" + uuid.NewString()})

	// 同包集成测试共用一个 testcontainer Postgres，数据不自动隔离：同包里有若干
	// 「今天」「最近 N 天」的无实体过滤计数断言（如 dashboard 的 ActiveUsers），
	// 用 time.Now() 落行会把它们的数目撑大。锚到一个固定且唯一的过去时刻即可避开
	// —— 本测试只做按 id 的往返读取，与时间窗无关。
	createdAt := time.Date(2023, 4, 11, 5, 6, 7, 0, time.UTC)
	newLog := func(phase service.PhaseLatency) *service.UsageLog {
		return &service.UsageLog{
			UserID:       user.ID,
			APIKeyID:     apiKey.ID,
			AccountID:    account.ID,
			RequestID:    uuid.NewString(),
			Model:        "claude-3",
			InputTokens:  10,
			OutputTokens: 5,
			TotalCost:    1.0,
			ActualCost:   1.0,
			PhaseLatency: phase,
			CreatedAt:    createdAt,
		}
	}

	auth, routing, upstream, response := 7, 19296, 29201, 460
	measured := newLog(service.PhaseLatency{
		AuthLatencyMs:     &auth,
		RoutingLatencyMs:  &routing,
		UpstreamLatencyMs: &upstream,
		ResponseLatencyMs: &response,
	})
	_, err := repo.Create(ctx, measured)
	require.NoError(t, err)
	require.NotZero(t, measured.ID)

	// 未插桩的平台路径：整组缺失。
	absent := newLog(service.PhaseLatency{})
	_, err = repo.Create(ctx, absent)
	require.NoError(t, err)

	// 亚毫秒的真实测量：0 必须留住，不能退化成 NULL。
	zero := 0
	subMs := newLog(service.PhaseLatency{
		AuthLatencyMs:     &zero,
		RoutingLatencyMs:  &zero,
		UpstreamLatencyMs: &zero,
		ResponseLatencyMs: &zero,
	})
	_, err = repo.Create(ctx, subMs)
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, measured.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AuthLatencyMs)
	require.Equal(t, auth, *got.AuthLatencyMs)
	require.NotNil(t, got.RoutingLatencyMs)
	require.Equal(t, routing, *got.RoutingLatencyMs)
	require.NotNil(t, got.UpstreamLatencyMs)
	require.Equal(t, upstream, *got.UpstreamLatencyMs)
	require.NotNil(t, got.ResponseLatencyMs)
	require.Equal(t, response, *got.ResponseLatencyMs)

	gotAbsent, err := repo.GetByID(ctx, absent.ID)
	require.NoError(t, err)
	require.Nil(t, gotAbsent.AuthLatencyMs)
	require.Nil(t, gotAbsent.RoutingLatencyMs)
	require.Nil(t, gotAbsent.UpstreamLatencyMs)
	require.Nil(t, gotAbsent.ResponseLatencyMs)

	gotZero, err := repo.GetByID(ctx, subMs.ID)
	require.NoError(t, err)
	require.NotNil(t, gotZero.AuthLatencyMs)
	require.Equal(t, 0, *gotZero.AuthLatencyMs)
	require.NotNil(t, gotZero.ResponseLatencyMs)
	require.Equal(t, 0, *gotZero.ResponseLatencyMs)
}
