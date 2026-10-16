package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// slotWaitPing 与 ConcurrencyHelper 默认的等槽 ping 格式一致。
const slotWaitPing = ":\n\n"

// writeSlotWaitPings 模拟 handler 排队等并发槽位期间写出的 SSE ping：提交 200 +
// SSE 响应头，写注释并把字节记进 gateway_stream_heartbeat_bytes。
func writeSlotWaitPings(t *testing.T, count int) func(*gin.Context) {
	return func(c *gin.Context) {
		t.Helper()
		c.Header("Content-Type", "text/event-stream")
		for i := 0; i < count; i++ {
			n, err := c.Writer.Write([]byte(slotWaitPing))
			require.NoError(t, err)
			RecordGatewayStreamHeartbeat(c, n)
			c.Writer.Flush()
		}
	}
}

func TestOpenAIAdjustedWrittenSizeExcludesSlotWaitPings(t *testing.T) {
	c, rec := newCompactBridgeTestContext(t, false)
	writeSlotWaitPings(t, 2)(c)
	require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	require.False(t, openAIStreamClientOutputStarted(c, false))

	_, err := c.Writer.Write([]byte("data: semantic\n\n"))
	require.NoError(t, err)
	require.Equal(t, len("data: semantic\n\n"), OpenAICompactKeepaliveAdjustedWrittenSize(c))
	require.True(t, openAIStreamClientOutputStarted(c, false))
	require.Equal(t, slotWaitPing+slotWaitPing+"data: semantic\n\n", rec.Body.String())
}

// #298：请求在开流前排队超过一个 ping 间隔、客户端已收到等槽 ping，上游开流后在
// 首个可见输出前发出过载。ping 不是语义输出，必须仍然换号；客户端只看到 ping。
func TestOpenAIStreamOverloadAfterSlotWaitPingsStillFailsOver(t *testing.T) {
	streams := map[string]struct {
		stream  string
		staging []bool
	}{
		// 只有生命周期事件就过载：暂存开关开、关都应换号。
		"lifecycle_only": {
			stream:  preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageOverload, preOutputStageFailed),
			staging: []bool{false, true},
		},
		// 线上 #295 形态（不可见结构事件后过载）：只有暂存开启时可换号。
		"invisible_structural_event": {
			stream:  preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageReasoning, preOutputStageOverload, preOutputStageFailed),
			staging: []bool{true},
		},
		"response_failed_only": {
			stream:  preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageFailed),
			staging: []bool{false, true},
		},
	}
	for name, tc := range streams {
		for _, path := range []string{"native", "passthrough"} {
			for _, enabled := range tc.staging {
				label := "disabled"
				cfg := defaultOpenAIPreOutputStagingSettings()
				if enabled {
					label = "enabled"
					cfg = enabledPreOutputStaging()
				}
				t.Run(name+"/"+path+"/"+label, func(t *testing.T) {
					useOpenAIPreOutputStaging(t, cfg)
					run := runPreOutputStageStreamWith(t, path, config.GatewayConfig{},
						io.NopCloser(strings.NewReader(tc.stream)), writeSlotWaitPings(t, 3))
					require.Error(t, run.err)
					var failoverErr *UpstreamFailoverError
					require.ErrorAs(t, run.err, &failoverErr)
					require.Equal(t, strings.Repeat(slotWaitPing, 3), run.body, "only the slot-wait pings may reach the client")
					require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(run.c))
				})
			}
		}
	}
}

// 反向：等槽 ping 之后已经向客户端写出可见输出，再过载时不能换号（否则会重复输出）。
func TestOpenAIStreamOverloadAfterSlotWaitPingsAndVisibleOutputIsDelivered(t *testing.T) {
	stream := preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageDelta, preOutputStageOverload, preOutputStageFailed)
	for _, path := range []string{"native", "passthrough"} {
		for _, enabled := range []bool{false, true} {
			cfg := defaultOpenAIPreOutputStagingSettings()
			label := "disabled"
			if enabled {
				cfg = enabledPreOutputStaging()
				label = "enabled"
			}
			t.Run(path+"/"+label, func(t *testing.T) {
				useOpenAIPreOutputStaging(t, cfg)
				run := runPreOutputStageStreamWith(t, path, config.GatewayConfig{},
					io.NopCloser(strings.NewReader(stream)), writeSlotWaitPings(t, 1))
				require.Error(t, run.err)
				var failoverErr *UpstreamFailoverError
				require.NotErrorAs(t, run.err, &failoverErr)
				require.True(t, strings.HasPrefix(run.body, slotWaitPing))
				require.Contains(t, run.body, "hello")
				require.Contains(t, run.body, "response.failed")
				require.Equal(t, http.StatusOK, run.c.Writer.Status())
			})
		}
	}
}
