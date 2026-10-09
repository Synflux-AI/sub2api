//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// sigCaptureUpstream 记录每次上游请求体，按顺序回放预置响应。
type sigCaptureUpstream struct {
	service.HTTPUpstream
	mu     sync.Mutex
	bodies []string
	resps  []sigCannedResp
}

type sigCannedResp struct {
	status int
	body   string
}

func (u *sigCaptureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies = append(u.bodies, string(b))
	r := u.resps[0]
	if len(u.resps) > 1 {
		u.resps = u.resps[1:]
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(r.body)),
	}, nil
}

const (
	realSigClient   = "UkVBTF9TSUdfRlJPTV9DTElFTlQ="     // 客户端历史里的真签名
	realSigUpstream = "UkVBTF9TSUdfRlJPTV9VUFNUUkVBTQ==" // 本轮上游返回的新签名
)

// 第 2 轮请求：历史里有一次并行调用（只有第 1 个 functionCall 带签名，和官方实测一致）。
var geminiSigHistoryBody = `{"contents":[` +
	`{"role":"user","parts":[{"text":"weather in Paris and Tokyo?"}]},` +
	`{"role":"model","parts":[` +
	`{"functionCall":{"name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"` + realSigClient + `"},` +
	`{"functionCall":{"name":"get_weather","args":{"city":"Tokyo"}}}]},` +
	`{"role":"user","parts":[` +
	`{"functionResponse":{"name":"get_weather","response":{"temp_c":18}}},` +
	`{"functionResponse":{"name":"get_weather","response":{"temp_c":22}}}]}]}`

var geminiSigOKResponse = `{"candidates":[{"content":{"role":"model","parts":[` +
	`{"functionCall":{"name":"get_weather","args":{"city":"Berlin"}},"thoughtSignature":"` + realSigUpstream + `"}]},` +
	`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3}}`

func serveGeminiSigRequest(t *testing.T, resps []sigCannedResp) (*sigCaptureUpstream, int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := newGeminiClientCancelFixture(t) // 单账号 Gemini 分组、无任何粘性绑定
	up := &sigCaptureUpstream{resps: resps}
	f.handler.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, up, nil, &config.Config{}, nil)
	rec, _ := f.serve(t,
		"/v1beta/models/*modelAction",
		"/v1beta/models/gemini-3.8-flash:generateContent",
		geminiSigHistoryBody,
		f.handler.GeminiV1BetaModels,
	)
	return up, rec.Code, rec.Body.String()
}

// 单账号分组、查不到粘性绑定：真签名必须原样转发，上游新签名必须原样回给客户端。
func TestGeminiV1BetaModels_SingleAccountNoBindingKeepsThoughtSignature(t *testing.T) {
	up, code, respBody := serveGeminiSigRequest(t, []sigCannedResp{{http.StatusOK, geminiSigOKResponse}})

	require.Equal(t, http.StatusOK, code)
	require.Len(t, up.bodies, 1)
	sent := up.bodies[0]
	require.Equal(t, realSigClient, gjson.Get(sent, "contents.1.parts.0.thoughtSignature").String(), "第 1 个 functionCall 的真签名被改写")
	// 并行调用的第 2 个 functionCall 官方本来就不带签名，补 dummy 不会 400、也不改 prompt token（官方实测）。
	require.Equal(t, "skip_thought_signature_validator", gjson.Get(sent, "contents.1.parts.1.thoughtSignature").String())
	require.Equal(t, realSigUpstream, gjson.Get(respBody, "candidates.0.content.parts.0.thoughtSignature").String())
}

// 签名真的不被上游认（换号、伪造）时：清洗后同号重试一次，客户端拿到 200。
func TestGeminiV1BetaModels_InvalidSignature400RetriesWithCleanedSignatures(t *testing.T) {
	invalid := `{"error":{"code":400,"message":"Invalid thought signature.","status":"INVALID_ARGUMENT"}}`
	up, code, _ := serveGeminiSigRequest(t, []sigCannedResp{{http.StatusBadRequest, invalid}, {http.StatusOK, geminiSigOKResponse}})

	require.Equal(t, http.StatusOK, code)
	require.Len(t, up.bodies, 2)
	require.Equal(t, realSigClient, gjson.Get(up.bodies[0], "contents.1.parts.0.thoughtSignature").String())
	require.Equal(t, "skip_thought_signature_validator", gjson.Get(up.bodies[1], "contents.1.parts.0.thoughtSignature").String())
	require.False(t, strings.Contains(up.bodies[1], realSigClient))
}

// 流式：真签名原样转发，SSE 里的上游签名原样回给客户端。
func TestGeminiV1BetaModels_StreamKeepsThoughtSignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newGeminiClientCancelFixture(t)
	up := &sigCaptureUpstream{resps: []sigCannedResp{{http.StatusOK, "data: " + geminiSigOKResponse + "\n\n"}}}
	f.handler.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, up, nil, &config.Config{}, nil)
	rec, _ := f.serve(t,
		"/v1beta/models/*modelAction",
		"/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
		geminiSigHistoryBody,
		f.handler.GeminiV1BetaModels,
	)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, up.bodies, 1)
	require.Equal(t, realSigClient, gjson.Get(up.bodies[0], "contents.1.parts.0.thoughtSignature").String())
	require.Contains(t, rec.Body.String(), realSigUpstream)
}
