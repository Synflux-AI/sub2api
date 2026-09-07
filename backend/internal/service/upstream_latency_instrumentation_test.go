//go:build unit

package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
)

type slowStubUpstream struct{ delay time.Duration }

func (s slowStubUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	time.Sleep(s.delay)
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"boom"}}`)),
	}, nil
}

// DoWithTLS satisfies the HTTPUpstream interface; timedUpstreamDo only exercises
// Do, but the interface itself requires both methods.
func (s slowStubUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

func TestTimedUpstreamDoRecordsLatency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/v1", nil)
	resp, err := timedUpstreamDo(c, slowStubUpstream{delay: 5 * time.Millisecond}, req, "", 1, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	if got := opsUpstreamLatencyMs(c); got <= 0 {
		t.Fatalf("expected a positive upstream latency, got %d — the rule engine's latency condition is fail-closed and would never fire", got)
	}
}

// 防漏回归：gemini / antigravity 的**请求转发**上游出口必须全部走 timedUpstreamDo。
// 逐点插桩必漏，而漏掉的路径上「上游耗时上限」条件永远不生效。
//
// instrumentationExempt 是显式豁免名单：不在请求转发路径上、拿不到 gin.Context 的
// 出口（如账号连通性测试）留在这里，附理由。名单只能因「确实拿不到 c」而增长，
// 不能因为「改起来麻烦」。
var instrumentationExempt = map[string]string{
	// 形如 "gemini_messages_compat_service.go:2870": "账号测试路径，无 gin.Context",
	"gemini_messages_compat_service.go:2870": "AI Studio GET 辅助路径，签名只有 context.Context，非请求转发路径",
}

func TestGeminiAndAntigravityUpstreamExitsAreInstrumented(t *testing.T) {
	files := []string{
		"gemini_messages_compat_service.go",
		"gemini_chat_completions_compat_service.go",
		"antigravity_gateway_upstream.go",
		"antigravity_gateway_retry.go",
	}
	bare := regexp.MustCompile(`\bhttpUpstream\.Do\(`)
	for _, name := range files {
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !bare.MatchString(line) {
				continue
			}
			site := fmt.Sprintf("%s:%d", name, i+1)
			if reason, ok := instrumentationExempt[site]; ok {
				t.Logf("%s exempt from instrumentation: %s", site, reason)
				continue
			}
			t.Errorf("%s still calls httpUpstream.Do directly; route it through timedUpstreamDo (or add it to instrumentationExempt with a reason): %s",
				site, strings.TrimSpace(line))
		}
	}
}
