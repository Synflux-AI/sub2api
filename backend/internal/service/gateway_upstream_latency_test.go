package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type latencyProbeUpstream struct {
	delay time.Duration
	err   error
}

func (u *latencyProbeUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	panic("not used")
}

func (u *latencyProbeUpstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	time.Sleep(u.delay)
	if u.err != nil {
		return nil, u.err
	}
	return &http.Response{StatusCode: http.StatusOK}, nil
}

func newLatencyProbeContext(t *testing.T) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, err := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	require.NoError(t, err)
	c.Request = req
	return c
}

func upstreamLatencyOf(t *testing.T, c *gin.Context) int64 {
	t.Helper()
	v, ok := c.Get(OpsUpstreamLatencyMsKey)
	require.True(t, ok, "upstream latency key must be set")
	ms, ok := v.(int64)
	require.True(t, ok, "upstream latency should be int64, got %T", v)
	return ms
}

// TestTimedGatewayUpstreamDoWithTLS_RecordsLatency 是这一层包装存在的理由：
// Anthropic 侧的上游出口散在 5 个文件、8 个调用点上（主转发 + 请求内重试、两个兼容端点、
// API Key 透传、Bedrock）。逐点插桩漏掉任何一个，那条路径的「上游」就是空的，而「响应」
// 会把整段上游等待吞掉 —— 瀑布图上「上游 0ms、响应顶满」，比没有数据更误导人。
func TestTimedGatewayUpstreamDoWithTLS_RecordsLatency(t *testing.T) {
	svc := &GatewayService{httpUpstream: &latencyProbeUpstream{delay: 40 * time.Millisecond}}
	c := newLatencyProbeContext(t)

	resp, err := svc.timedGatewayUpstreamDoWithTLS(c, c.Request, "", 1, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, resp)

	got := upstreamLatencyOf(t, c)
	require.GreaterOrEqual(t, got, int64(30))
	require.Less(t, got, int64(400))
}

// 上游失败也要留下数字：上游超时正是最需要这个数字的场景，
// 把 Set 放在 err 判断之后就恰好丢掉了它。
func TestTimedGatewayUpstreamDoWithTLS_RecordsOnError(t *testing.T) {
	svc := &GatewayService{httpUpstream: &latencyProbeUpstream{
		delay: 30 * time.Millisecond,
		err:   errors.New("upstream refused"),
	}}
	c := newLatencyProbeContext(t)

	resp, err := svc.timedGatewayUpstreamDoWithTLS(c, c.Request, "", 1, 1, nil)
	require.Error(t, err)
	require.Nil(t, resp)

	got := upstreamLatencyOf(t, c)
	require.GreaterOrEqual(t, got, int64(20))
}

// 请求内多次尝试（failover / 400 重试 / 预算重试）会互相覆盖，留下最后一次的数字。
// 这与 OpenAI 系及 ops_error_logs 的既有口径一致，钉住以免无意改动。
func TestTimedGatewayUpstreamDoWithTLS_LastAttemptWins(t *testing.T) {
	svc := &GatewayService{httpUpstream: &latencyProbeUpstream{delay: 60 * time.Millisecond}}
	c := newLatencyProbeContext(t)

	_, err := svc.timedGatewayUpstreamDoWithTLS(c, c.Request, "", 1, 1, nil)
	require.NoError(t, err)
	first := upstreamLatencyOf(t, c)

	svc.httpUpstream = &latencyProbeUpstream{delay: 1 * time.Millisecond}
	_, err = svc.timedGatewayUpstreamDoWithTLS(c, c.Request, "", 1, 1, nil)
	require.NoError(t, err)
	second := upstreamLatencyOf(t, c)

	require.Less(t, second, first, "最后一次尝试的数字应当覆盖先前的")
}
