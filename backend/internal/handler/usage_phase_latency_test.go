package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestPhaseLatencyFromContext_ReadsAllFourKeys 把快照与 SetOpsLatencyMs 写入的 key 对齐。
// 取错 key 不会报错，只会让某一列永远为空 —— 只有逐 key 断言才能挡住。
func TestPhaseLatencyFromContext_ReadsAllFourKeys(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, 7)
	service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, 19296)
	service.SetOpsLatencyMs(c, service.OpsUpstreamLatencyMsKey, 29201)
	service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, 460)

	got := phaseLatencyFromContext(c)
	require.Equal(t, 7, *got.AuthLatencyMs)
	require.Equal(t, 19296, *got.RoutingLatencyMs)
	require.Equal(t, 29201, *got.UpstreamLatencyMs)
	require.Equal(t, 460, *got.ResponseLatencyMs)
}

// 未插桩的平台路径（如当前的 Claude 原生 /v1/messages）四个 key 都没设：整组为 nil，
// 落库成 NULL。补 0 会把「没测到」伪装成「0ms」。
func TestPhaseLatencyFromContext_NilWhenKeysAbsent(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	got := phaseLatencyFromContext(c)
	require.Nil(t, got.AuthLatencyMs)
	require.Nil(t, got.RoutingLatencyMs)
	require.Nil(t, got.UpstreamLatencyMs)
	require.Nil(t, got.ResponseLatencyMs)

	require.Equal(t, service.PhaseLatency{}, phaseLatencyFromContext(nil),
		"nil context must degrade to an empty snapshot, not panic")
}

// 亚毫秒阶段测出来就是 0ms，必须落成 0 而不是 nil。
func TestPhaseLatencyFromContext_KeepsMeasuredZero(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, 0)

	got := phaseLatencyFromContext(c)
	require.NotNil(t, got.AuthLatencyMs)
	require.Equal(t, 0, *got.AuthLatencyMs)
	require.Nil(t, got.RoutingLatencyMs)
}
