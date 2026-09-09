package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stubAccountHealthCache 只回放预置的原始分记录，供列表接口读取。
type stubAccountHealthCache struct {
	entries map[int64]service.HealthScoreEntry
}

func (c *stubAccountHealthCache) ApplyDelta(context.Context, int64, float64, int, int) (float64, error) {
	return 0, nil
}

func (c *stubAccountHealthCache) GetScoresBatch(_ context.Context, accountIDs []int64) (map[int64]service.HealthScoreEntry, error) {
	result := make(map[int64]service.HealthScoreEntry)
	for _, id := range accountIDs {
		if entry, ok := c.entries[id]; ok {
			result[id] = entry
		}
	}
	return result, nil
}

// stubAccountTTFTCache 只回放预置的 TTFT 快照。
type stubAccountTTFTCache struct {
	snapshots map[int64]*service.AccountTTFTSnapshot
}

func (c *stubAccountTTFTCache) SaveSnapshots(context.Context, map[int64]*service.AccountTTFTSnapshot, int) error {
	return nil
}

func (c *stubAccountTTFTCache) GetSnapshotsBatch(_ context.Context, accountIDs []int64) (map[int64]*service.AccountTTFTSnapshot, error) {
	result := make(map[int64]*service.AccountTTFTSnapshot)
	for _, id := range accountIDs {
		if snapshot, ok := c.snapshots[id]; ok {
			result[id] = snapshot
		}
	}
	return result, nil
}

func newAccountListHealthConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.HealthScoringEnabled = true
	cfg.Gateway.Scheduling.HealthDegradedThreshold = 70
	cfg.Gateway.Scheduling.HealthCircuitThreshold = 30
	cfg.Gateway.Scheduling.HealthRecoveryHalflifeSeconds = 600
	cfg.Gateway.Scheduling.HealthTTLSeconds = 1800
	return cfg
}

// setupAccountListHealthRouter 构造一个健康分开关打开、且带 TTFT 快照的账号列表路由。
func setupAccountListHealthRouter(
	healthEntries map[int64]service.HealthScoreEntry,
	ttftSnapshots map[int64]*service.AccountTTFTSnapshot,
) (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	cfg := newAccountListHealthConfig()
	rateLimitService := service.NewRateLimitService(nil, nil, cfg, nil, nil)
	rateLimitService.SetAccountHealthService(service.NewAccountHealthService(&stubAccountHealthCache{entries: healthEntries}, cfg, nil))

	router := gin.New()
	adminSvc := newStubAdminService()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, rateLimitService, nil, nil, nil, nil, nil, nil,
		&stubAccountTTFTCache{snapshots: ttftSnapshots}, nil)
	router.GET("/api/v1/admin/accounts", handler.List)
	return router, adminSvc
}

func accountListHealthAccounts(now time.Time) []service.Account {
	return []service.Account{
		{
			ID: 501, Name: "degraded-account", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 4,
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: 502, Name: "healthy-account", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 4,
			CreatedAt: now, UpdatedAt: now,
		},
	}
}

func accountListItemsByID(t *testing.T, body []byte) map[int64]map[string]any {
	t.Helper()
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	items := make(map[int64]map[string]any, len(payload.Data.Items))
	for _, item := range payload.Data.Items {
		id, ok := item["id"].(float64)
		require.True(t, ok, "item is missing a numeric id")
		items[int64(id)] = item
	}
	return items
}

// lite=1 是账号管理页唯一的列表请求形态，精简 DTO 必须保留健康分/分层/TTFT，
// 否则「健康分」「首 Token 时延」两列恒显示 "-"（#235）。
func TestAccountHandlerListLiteKeepsHealthScoreAndTTFT(t *testing.T) {
	now := time.Now().UTC()
	snapshot := &service.AccountTTFTSnapshot{
		AccountID: 501, P50Ms: 820, P95Ms: 2400, Samples: 37, Ratio: 1.8,
		WorstModel: "claude-sonnet-4-5", WorstRatio: 2.6, Degraded: true, UpdatedAt: now,
	}
	router, adminSvc := setupAccountListHealthRouter(
		map[int64]service.HealthScoreEntry{501: {Score: 42, UpdatedAt: now}},
		map[int64]*service.AccountTTFTSnapshot{501: snapshot},
	)
	adminSvc.accounts = accountListHealthAccounts(now)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	items := accountListItemsByID(t, rec.Body.Bytes())
	require.Len(t, items, 2)

	degraded := items[501]
	require.Contains(t, degraded, "health_score")
	require.InDelta(t, 42.0, degraded["health_score"], 0.5)
	require.Equal(t, float64(service.HealthTierDegraded), degraded["health_tier"])
	ttft, ok := degraded["ttft"].(map[string]any)
	require.True(t, ok, "lite response must carry the ttft snapshot")
	require.Equal(t, float64(820), ttft["p50_ms"])
	require.Equal(t, float64(37), ttft["samples"])
	require.Equal(t, "claude-sonnet-4-5", ttft["worst_model"])

	// 无健康分记录 = 满分主池；health_tier 为 0 时不能被 omitempty 吞掉，
	// 否则前端 healthTierLabel 拿不到分层。
	healthy := items[502]
	require.Equal(t, float64(100), healthy["health_score"])
	require.Equal(t, float64(service.HealthTierHealthy), healthy["health_tier"])
	require.NotContains(t, healthy, "ttft")
}

// 健康分关闭时精简响应不应凭空多出这几个字段。
func TestAccountHandlerListLiteOmitsHealthWhenDisabled(t *testing.T) {
	router, adminSvc := setupAccountListRouter()
	adminSvc.accounts = accountListHealthAccounts(time.Now().UTC())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	items := accountListItemsByID(t, rec.Body.Bytes())
	require.Len(t, items, 2)
	for id, item := range items {
		require.NotContains(t, item, "health_score", "account %d", id)
		require.NotContains(t, item, "health_tier", "account %d", id)
		require.NotContains(t, item, "ttft", "account %d", id)
	}
}

// 满分账号的健康分不随时间漂移，ETag 因此仍然稳定，自动刷新的 304 增量同步不受影响。
func TestAccountHandlerListLiteHealthKeepsETagStable(t *testing.T) {
	now := time.Now().UTC()
	router, adminSvc := setupAccountListHealthRouter(nil, nil)
	adminSvc.accounts = accountListHealthAccounts(now)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)

	rec304 := httptest.NewRecorder()
	req304 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	req304.Header.Set("If-None-Match", etag)
	router.ServeHTTP(rec304, req304)
	require.Equal(t, http.StatusNotModified, rec304.Code)
}

// 衰减中的健康分是按 time.Now() 连续变化的浮点数。若原样进 payload，列表 ETag
// 每次轮询都会变，自动刷新的 304 快路径对整页所有管理员失效——包括没开健康分列的。
// 下发前取整即可让 ETag 只在整数位变化时失效。
func TestAccountHandlerListLiteDecayingHealthKeepsETagStable(t *testing.T) {
	now := time.Now().UTC()
	// 半衰期 600s、已过 90s：100 - 58*0.5^0.15 ≈ 47.72，取整 48，离进位边界足够远。
	entries := map[int64]service.HealthScoreEntry{501: {Score: 42, UpdatedAt: now.Add(-90 * time.Second)}}
	router, adminSvc := setupAccountListHealthRouter(entries, nil)
	adminSvc.accounts = accountListHealthAccounts(now)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)

	items := accountListItemsByID(t, rec.Body.Bytes())
	require.Equal(t, float64(48), items[501]["health_score"], "下发的健康分必须是整数")

	// 同一条 Redis 记录、只是又过了一点时间：衰减值已经变了，但取整后没变，
	// ETag 必须仍然命中 304。
	rec304 := httptest.NewRecorder()
	req304 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	req304.Header.Set("If-None-Match", etag)
	router.ServeHTTP(rec304, req304)
	require.Equal(t, http.StatusNotModified, rec304.Code)
}

// 健康分变化必须改变 ETag，否则前端会一直拿到 304 而看到过期的分数。
func TestAccountHandlerListLiteHealthChangeInvalidatesETag(t *testing.T) {
	now := time.Now().UTC()
	entries := map[int64]service.HealthScoreEntry{501: {Score: 42, UpdatedAt: now}}
	router, adminSvc := setupAccountListHealthRouter(entries, nil)
	adminSvc.accounts = accountListHealthAccounts(now)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)

	entries[501] = service.HealthScoreEntry{Score: 12, UpdatedAt: now}

	recNext := httptest.NewRecorder()
	reqNext := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	reqNext.Header.Set("If-None-Match", etag)
	router.ServeHTTP(recNext, reqNext)
	require.Equal(t, http.StatusOK, recNext.Code)
	require.NotEqual(t, etag, recNext.Header().Get("ETag"))

	items := accountListItemsByID(t, recNext.Body.Bytes())
	require.InDelta(t, 12.0, items[501]["health_score"], 0.5)
	require.Equal(t, float64(service.HealthTierProbation), items[501]["health_tier"])
}
