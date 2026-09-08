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
// 文件名单本身也是历史上漏过一个真实转发出口的地方（Task 5 fix round 1：
// antigravity_gateway_gemini.go 里模型兜底重试的 httpUpstream.Do 不在最初手写的
// 4 个文件名单里，测试扫不到它）。改成按目录枚举 antigravity_ / gemini_ 前缀文件，
// 让"文件名单本身不全"这一类缺口不再需要靠人记全。
//
// instrumentationExempt 是显式豁免名单：出口留在这里必须满足以下两类之一，并附理由：
//  1. 不在请求转发路径上、拿不到 gin.Context（如账号连通性测试）；
//  2. 拿得到 gin.Context，但这次 Do 调用不是本次推理请求的上游出口 —— 例如探测/
//     校验类的旁路请求。把它的耗时写进 OpsUpstreamLatencyMsKey 会覆盖真正上游调用
//     的耗时；而该条件对"没有耗时数据"是 fail-closed（不满足任何阈值），对"耗时
//     数据存在但是错的"却是 fail-open（一个碰巧不大的错误数字会被判定满足阈值）——
//     写错比不写更危险，所以宁可留空也不能瞎写。
//
// 名单只能因以上两条之一而增长，不能因为「改起来麻烦」。
var instrumentationExempt = map[string]string{
	"gemini_messages_compat_service.go:2939": "AI Studio GET 辅助路径，签名只有 context.Context，非请求转发路径",
}

func TestGeminiAndAntigravityUpstreamExitsAreInstrumented(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, "_test.go") || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasPrefix(name, "antigravity_") || strings.HasPrefix(name, "gemini_") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		t.Fatal("no antigravity_/gemini_ files found — directory enumeration is broken")
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
