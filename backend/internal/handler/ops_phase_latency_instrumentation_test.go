package handler

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func responseLatencyOf(t *testing.T, c *gin.Context) int64 {
	t.Helper()
	v, ok := c.Get(service.OpsResponseLatencyMsKey)
	require.True(t, ok, "response latency key must be set")
	ms, ok := v.(int64)
	require.True(t, ok, "response latency should be int64, got %T", v)
	return ms
}

// TestSetOpsResponseLatencyMs_SubtractsUpstream 是这次插桩的核心口径：
// 「响应」= Forward 总耗时 − 上游取响应头耗时。不减的话上游的等待会被算进响应，
// 瀑布图上「上游」是 0、「响应」顶满 —— 比没有数据更误导人。
func TestSetOpsResponseLatencyMs_SubtractsUpstream(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetOpsLatencyMs(c, service.OpsUpstreamLatencyMsKey, 120)

	forwardStart := time.Now().Add(-500 * time.Millisecond)
	setOpsResponseLatencyMs(c, forwardStart)

	got := responseLatencyOf(t, c)
	require.Greater(t, got, int64(300), "500ms forward minus 120ms upstream should leave ~380ms")
	require.Less(t, got, int64(450))
}

// 上游耗时缺失（该平台路径未插桩，或请求在拿到响应头前就失败）：整段记为响应。
// 宁可让「响应」偏大，也不猜一个上游耗时填进去。
func TestSetOpsResponseLatencyMs_FallsBackToWholeForward(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	setOpsResponseLatencyMs(c, time.Now().Add(-200*time.Millisecond))

	got := responseLatencyOf(t, c)
	require.Greater(t, got, int64(150))
	require.Less(t, got, int64(300))
}

// 上游耗时不小于 Forward 总耗时时不做减法（时钟粒度或上游阶段跨越了重试边界），
// 否则会得到负值或 0，读起来像「响应瞬间完成」。
func TestSetOpsResponseLatencyMs_NoSubtractionWhenUpstreamNotSmaller(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetOpsLatencyMs(c, service.OpsUpstreamLatencyMsKey, 9_000)

	setOpsResponseLatencyMs(c, time.Now().Add(-50*time.Millisecond))

	got := responseLatencyOf(t, c)
	require.GreaterOrEqual(t, got, int64(0))
	require.Less(t, got, int64(200), "must fall back to the forward duration, not go negative")
}

// TestPhaseLatencyKeys_ClaudeNativeCoverage 钉住这次补插桩的四个 key 都能被读出来。
// Claude 原生路径此前一个都没设，错误详情抽屉的「请求时序」卡对它恒为空。
func TestPhaseLatencyKeys_ClaudeNativeCoverage(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	for _, key := range []string{
		service.OpsAuthLatencyMsKey,
		service.OpsRoutingLatencyMsKey,
		service.OpsUpstreamLatencyMsKey,
		service.OpsResponseLatencyMsKey,
	} {
		require.Nil(t, getContextLatencyMs(c, key), "%s should start empty", key)
		service.SetOpsLatencyMs(c, key, 42)
		got := getContextLatencyMs(c, key)
		require.NotNil(t, got, "%s must be readable after SetOpsLatencyMs", key)
		require.Equal(t, int64(42), *got)
	}
}

// TestSetOpsRoutingLatencyMsIfAbsent_RecordsOnEarlyExit 覆盖路由阶段中途失败：
// 并发槽超时、余额复核不过、没有可用账号都会在 Forward 之前 return，
// 此前这些错误一个路由数字都没有 —— 恰恰是最需要路由诊断的场景。
func TestSetOpsRoutingLatencyMsIfAbsent_RecordsOnEarlyExit(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	setOpsRoutingLatencyMsIfAbsent(c, time.Now().Add(-300*time.Millisecond))

	got := getContextLatencyMs(c, service.OpsRoutingLatencyMsKey)
	require.NotNil(t, got)
	require.Greater(t, *got, int64(250))
	require.Less(t, *got, int64(400))
}

// 正常路径在 Forward 之前已显式记过「到 Forward 为止」的跨度，函数退出时的兜底
// 绝不能覆盖它 —— 否则整个 Forward 都会被算进路由，上游耗时凭空出现在路由里。
func TestSetOpsRoutingLatencyMsIfAbsent_DoesNotOverwrite(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, 42)

	setOpsRoutingLatencyMsIfAbsent(c, time.Now().Add(-5*time.Second))

	got := getContextLatencyMs(c, service.OpsRoutingLatencyMsKey)
	require.NotNil(t, got)
	require.Equal(t, int64(42), *got, "已有值必须原样保留")
}

func TestSetOpsRoutingLatencyMsIfAbsent_NilContext(t *testing.T) {
	require.NotPanics(t, func() { setOpsRoutingLatencyMsIfAbsent(nil, time.Now()) })
}
