package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var urlPolicyJPEGBytes = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1}

// mediaUpstreamStub 按 URL 返回图片，并发安全，记录每次下载请求。
type mediaUpstreamStub struct {
	mu       sync.Mutex
	requests []*http.Request
	proxies  []string
	handler  func(req *http.Request) (*http.Response, error)
}

func (s *mediaUpstreamStub) Do(req *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.proxies = append(s.proxies, proxyURL)
	s.mu.Unlock()
	if s.handler == nil {
		return b64BackfillImageResponse(http.StatusOK, "image/png", b64BackfillPNGBytes), nil
	}
	return s.handler(req)
}

func (s *mediaUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

func (s *mediaUpstreamStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

type rehostedImage struct {
	key         string
	contentType string
	data        []byte
}

type memoryImageStorage struct {
	mu    sync.Mutex
	saved []rehostedImage
	err   error
}

func (s *memoryImageStorage) Save(_ context.Context, key, contentType string, data []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.mu.Lock()
	s.saved = append(s.saved, rehostedImage{key: key, contentType: contentType, data: append([]byte(nil), data...)})
	s.mu.Unlock()
	return "https://img.example.com/" + key, nil
}

func newURLPolicyService(upstream HTTPUpstream, policy ImageURLPolicy) *OpenAIGatewayService {
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	if policy.Mode != "" {
		svc.SetImageURLPolicyResolver(func() ImageURLPolicy { return policy })
	}
	return svc
}

func rewriteForTest(t *testing.T, svc *OpenAIGatewayService, account *Account, parsed *OpenAIImagesRequest, body string) []byte {
	t.Helper()
	policy, ok := svc.activeImageURLPolicy(context.Background(), nil)
	if !ok {
		return []byte(body)
	}
	out, _, _ := svc.rewriteOpenAIImagesResponseURLs(context.Background(), account, parsed, policy, []byte(body))
	return out
}

func TestOpenAIImagesURLPolicy_InlineMatrix(t *testing.T) {
	pngB64 := base64.StdEncoding.EncodeToString(b64BackfillPNGBytes)
	pngDataURL := "data:image/png;base64," + pngB64
	urlOnly := `{"created":1,"data":[{"url":"https://cdn.example.com/a.png","revised_prompt":"a cat"}]}`
	both := `{"created":1,"data":[{"url":"https://cdn.example.com/a.png","b64_json":"` + pngB64 + `"}]}`
	b64Only := `{"created":1,"data":[{"b64_json":"` + pngB64 + `"}]}`

	tests := []struct {
		name          string
		format        string
		backfill      bool
		body          string
		wantURL       string
		wantB64       string
		wantDownloads int
	}{
		{name: "显式 url × 上游 url → data URL", format: "url", body: urlOnly, wantURL: pngDataURL, wantDownloads: 1},
		{name: "显式 url × 上游双字段 → data URL 且删 b64", format: "url", body: both, wantURL: pngDataURL},
		{name: "显式 b64 × 上游 url → b64_json 且删 url", format: "b64_json", body: urlOnly, wantB64: pngB64, wantDownloads: 1},
		{name: "显式 b64 × 上游双字段 → 删 url", format: "b64_json", body: both, wantB64: pngB64},
		{name: "未指定 × 上游 url → 跟随上游保留 url 字段", body: urlOnly, wantURL: pngDataURL, wantDownloads: 1},
		{name: "未指定 × 上游 b64 → 不动", body: b64Only, wantB64: pngB64},
		{name: "未指定 × 上游双字段 → 删 url 保留 b64", body: both, wantB64: pngB64},
		{name: "未指定 × 账号开回填 → 视为偏好 b64", backfill: true, body: urlOnly, wantB64: pngB64, wantDownloads: 1},
		{name: "显式 url × 账号开回填 → 仍按 url", format: "url", backfill: true, body: urlOnly, wantURL: pngDataURL, wantDownloads: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &mediaUpstreamStub{}
			svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeInline})
			got := rewriteForTest(t, svc, b64BackfillAccount(tt.backfill), &OpenAIImagesRequest{ResponseFormat: tt.format}, tt.body)

			require.Equal(t, tt.wantURL, gjson.GetBytes(got, "data.0.url").String())
			require.Equal(t, tt.wantB64, gjson.GetBytes(got, "data.0.b64_json").String())
			require.Equal(t, tt.wantURL != "", gjson.GetBytes(got, "data.0.url").Exists())
			require.Equal(t, tt.wantB64 != "", gjson.GetBytes(got, "data.0.b64_json").Exists())
			require.NotContains(t, string(got), "cdn.example.com")
			require.Equal(t, tt.wantDownloads, upstream.count())
		})
	}
}

func TestOpenAIImagesURLPolicy_Rehost(t *testing.T) {
	upstream := &mediaUpstreamStub{handler: func(*http.Request) (*http.Response, error) {
		// 响应头谎报为 png，contentType 必须以嗅探结果为准。
		return b64BackfillImageResponse(http.StatusOK, "image/png", urlPolicyJPEGBytes), nil
	}}
	storage := &memoryImageStorage{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeRehost, Storage: storage, Prefix: "gen/", MaxDownloadBytes: 1 << 20})
	account := b64BackfillAccount(false)
	proxyID := int64(3)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 7890}

	got := rewriteForTest(t, svc, account, &OpenAIImagesRequest{ResponseFormat: "url"},
		`{"created":1,"data":[{"url":"https://cdn.example.com/a.png?sig=abc","b64_json":""}]}`)

	require.Len(t, storage.saved, 1)
	saved := storage.saved[0]
	require.Regexp(t, regexp.MustCompile(`^gen/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jpg$`), saved.key)
	require.Equal(t, "image/jpeg", saved.contentType)
	require.Equal(t, urlPolicyJPEGBytes, saved.data)
	require.Equal(t, "https://img.example.com/"+saved.key, gjson.GetBytes(got, "data.0.url").String())
	require.False(t, gjson.GetBytes(got, "data.0.b64_json").Exists())

	require.Len(t, upstream.requests, 1)
	req := upstream.requests[0]
	require.Equal(t, "https://cdn.example.com/a.png?sig=abc", req.URL.String(), "下载用原始 url，不做规范化")
	require.Equal(t, "http://127.0.0.1:7890", upstream.proxies[0])
	_, hasDeadline := req.Context().Deadline()
	require.True(t, hasDeadline)
	require.True(t, HTTPUpstreamPublicHostsOnly(req.Context()), "重定向每一跳都必须校验公网")
}

func TestOpenAIImagesURLPolicy_RehostUploadsUpstreamB64WhenURLRequested(t *testing.T) {
	pngB64 := base64.StdEncoding.EncodeToString(b64BackfillPNGBytes)
	upstream := &mediaUpstreamStub{}
	storage := &memoryImageStorage{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeRehost, Storage: storage, Prefix: "images/", MaxDownloadBytes: 1 << 20})

	got := rewriteForTest(t, svc, b64BackfillAccount(false), &OpenAIImagesRequest{ResponseFormat: "url"},
		`{"data":[{"url":"https://cdn.example.com/a.png","b64_json":"`+pngB64+`"}]}`)

	require.Zero(t, upstream.count(), "已有图片字节时不下载")
	require.Len(t, storage.saved, 1)
	require.Equal(t, "image/png", storage.saved[0].contentType)
	require.Equal(t, "https://img.example.com/"+storage.saved[0].key, gjson.GetBytes(got, "data.0.url").String())
	require.False(t, gjson.GetBytes(got, "data.0.b64_json").Exists())
}

func TestOpenAIImagesURLPolicy_FailureMatrix(t *testing.T) {
	const upstreamURL = "https://cdn.example.com/a.png"
	tooLarge := append(append([]byte(nil), b64BackfillPNGBytes...), bytes.Repeat([]byte{0}, 64)...)
	tests := []struct {
		name          string
		resp          func() (*http.Response, error)
		storageErr    error
		wantKeepURL   bool
		wantDataURL   bool
		wantDownloads int
	}{
		{name: "下载 404 保留上游 url", resp: func() (*http.Response, error) {
			return b64BackfillImageResponse(http.StatusNotFound, "text/plain", []byte("gone")), nil
		}, wantKeepURL: true, wantDownloads: 1},
		{name: "下载超时/传输错误保留上游 url", resp: func() (*http.Response, error) {
			return nil, context.DeadlineExceeded
		}, wantKeepURL: true, wantDownloads: 1},
		{name: "超过 max_download_bytes 保留上游 url", resp: func() (*http.Response, error) {
			return b64BackfillImageResponse(http.StatusOK, "image/png", tooLarge), nil
		}, wantKeepURL: true, wantDownloads: 1},
		{name: "内容不是图片保留上游 url", resp: func() (*http.Response, error) {
			return b64BackfillImageResponse(http.StatusOK, "image/png", []byte("<html>login</html>")), nil
		}, wantKeepURL: true, wantDownloads: 1},
		{name: "上传失败退化为 data URL", storageErr: errors.New("s3 down"), wantDataURL: true, wantDownloads: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &mediaUpstreamStub{}
			if tt.resp != nil {
				upstream.handler = func(*http.Request) (*http.Response, error) { return tt.resp() }
			}
			storage := &memoryImageStorage{err: tt.storageErr}
			svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeRehost, Storage: storage, Prefix: "images/", MaxDownloadBytes: int64(len(b64BackfillPNGBytes) + 8)})
			got := rewriteForTest(t, svc, b64BackfillAccount(false), &OpenAIImagesRequest{ResponseFormat: "url"},
				`{"data":[{"url":"`+upstreamURL+`"}]}`)

			gotURL := gjson.GetBytes(got, "data.0.url").String()
			if tt.wantKeepURL {
				require.Equal(t, upstreamURL, gotURL, "失败时绝不删除唯一的图片字段")
			}
			if tt.wantDataURL {
				require.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(b64BackfillPNGBytes), gotURL)
			}
			require.Equal(t, tt.wantDownloads, upstream.count())
		})
	}
}

func TestOpenAIImagesURLPolicy_RejectsPrivateHostsWithoutRequest(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1/a.png",
		"http://localhost:8080/a.png",
		"http://10.0.0.8/a.png",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/a.png",
	} {
		t.Run(rawURL, func(t *testing.T) {
			upstream := &mediaUpstreamStub{}
			svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeInline})
			got := rewriteForTest(t, svc, b64BackfillAccount(false), nil, `{"data":[{"url":"`+rawURL+`"}]}`)
			require.Equal(t, rawURL, gjson.GetBytes(got, "data.0.url").String())
			require.Zero(t, upstream.count())
		})
	}
}

func TestOpenAIImagesURLPolicy_MediaHostsAllowlist(t *testing.T) {
	upstream := &mediaUpstreamStub{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeInline})
	// upstream_hosts 不参与媒体下载：即便开了白名单且不含图片 CDN，也只按 media_hosts 判定。
	svc.cfg.Security.URLAllowlist = config.URLAllowlistConfig{
		Enabled:       true,
		UpstreamHosts: []string{"api.openai.com"},
		MediaHosts:    []string{"*.example.com"},
	}

	got := rewriteForTest(t, svc, b64BackfillAccount(false), nil,
		`{"data":[{"url":"https://cdn.example.com/a.png"},{"url":"https://cdn.other.test/b.png"},{"url":"http://cdn.example.com/c.png"}]}`)

	require.True(t, strings.HasPrefix(gjson.GetBytes(got, "data.0.url").String(), "data:image/png;base64,"))
	require.Equal(t, "https://cdn.other.test/b.png", gjson.GetBytes(got, "data.1.url").String(), "不在 media_hosts 内不下载")
	require.Equal(t, "http://cdn.example.com/c.png", gjson.GetBytes(got, "data.2.url").String(), "开白名单后只允许 https")
	require.Equal(t, 1, upstream.count())
}

func TestOpenAIImagesURLPolicy_MultiImageKeepsOrderAndIsolatesFailures(t *testing.T) {
	upstream := &mediaUpstreamStub{handler: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/bad.png":
			return b64BackfillImageResponse(http.StatusForbidden, "text/plain", []byte("denied")), nil
		case "/jpeg.jpg":
			return b64BackfillImageResponse(http.StatusOK, "image/jpeg", urlPolicyJPEGBytes), nil
		default:
			return b64BackfillImageResponse(http.StatusOK, "image/png", b64BackfillPNGBytes), nil
		}
	}}
	storage := &memoryImageStorage{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeRehost, Storage: storage, Prefix: "images/", MaxDownloadBytes: 1 << 20})

	var data []string
	paths := []string{"/0.png", "/jpeg.jpg", "/bad.png", "/3.png", "/4.png", "/5.png", "/6.png", "/7.png", "/8.png", "/9.png"}
	for _, p := range paths {
		data = append(data, `{"url":"https://cdn.example.com`+p+`","revised_prompt":"`+p+`"}`)
	}
	got := rewriteForTest(t, svc, b64BackfillAccount(false), nil, `{"data":[`+strings.Join(data, ",")+`]}`)

	require.Equal(t, len(paths), upstream.count())
	require.Len(t, storage.saved, len(paths)-1)
	for i, p := range paths {
		require.Equal(t, p, gjson.GetBytes(got, "data."+strconv.Itoa(i)+".revised_prompt").String(), "索引顺序保持")
		gotURL := gjson.GetBytes(got, "data."+strconv.Itoa(i)+".url").String()
		if p == "/bad.png" {
			require.Equal(t, "https://cdn.example.com/bad.png", gotURL)
			continue
		}
		require.True(t, strings.HasPrefix(gotURL, "https://img.example.com/images/"), gotURL)
	}
	require.True(t, strings.HasSuffix(gjson.GetBytes(got, "data.1.url").String(), ".jpg"))
}

func TestOpenAIImagesURLPolicy_InlineBudget(t *testing.T) {
	big := append(append([]byte(nil), b64BackfillPNGBytes...), bytes.Repeat([]byte{1}, int(openAIImageURLInlineBudgetBytes/2))...)
	upstream := &mediaUpstreamStub{handler: func(*http.Request) (*http.Response, error) {
		return b64BackfillImageResponse(http.StatusOK, "image/png", big), nil
	}}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeInline, MaxDownloadBytes: int64(len(big))})
	got := rewriteForTest(t, svc, b64BackfillAccount(false), nil,
		`{"data":[{"url":"https://cdn.example.com/0.png"},{"url":"https://cdn.example.com/1.png"},{"url":"https://cdn.example.com/2.png"}]}`)

	inlined, kept := 0, 0
	for i := 0; i < 3; i++ {
		if strings.HasPrefix(gjson.GetBytes(got, "data."+strconv.Itoa(i)+".url").String(), "data:") {
			inlined++
		} else {
			kept++
		}
	}
	require.Equal(t, 1, inlined, "单个响应内联总字节受预算约束")
	require.Equal(t, 2, kept)
}

func TestOpenAIImagesURLPolicy_SkipsDataURLsAndNonHTTP(t *testing.T) {
	upstream := &mediaUpstreamStub{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeInline})
	body := `{"data":[{"url":"data:image/png;base64,aW1n"},{"url":"ftp://cdn.example.com/a.png"},{"revised_prompt":"x"}]}`
	got := rewriteForTest(t, svc, b64BackfillAccount(false), nil, body)
	require.Equal(t, body, string(got))
	require.Zero(t, upstream.count())
}

// 下面经过真实的非流式 handler，校验开关默认值、计费数据与响应头。

func runImagesNonStreaming(t *testing.T, svc *OpenAIGatewayService, account *Account, parsed *OpenAIImagesRequest, body string, reqCtx context.Context) (*httptest.ResponseRecorder, int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(reqCtx)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("ETag", `"upstream-etag"`)
	header.Set("Content-MD5", "upstream-md5")
	resp := &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}
	_, count, _, format, err := svc.handleOpenAIImagesNonStreamingResponse(reqCtx, resp, c, account, "https://relay.example.com/v1/images/generations", parsed)
	require.NoError(t, err)
	return rec, count, format
}

func TestOpenAIImagesURLPolicy_DefaultIsByteForBytePassthrough(t *testing.T) {
	body := `{"created":1,"data":[{"url":"https://cdn.example.com/a.png"}],"usage":{"input_tokens":3}}`
	for _, tc := range []struct {
		name   string
		policy ImageURLPolicy
	}{
		{name: "未注入设置"},
		{name: "passthrough 即使配了存储", policy: ImageURLPolicy{Mode: ImageURLModePassthrough, Storage: &memoryImageStorage{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &mediaUpstreamStub{}
			svc := newURLPolicyService(upstream, tc.policy)
			rec, count, format := runImagesNonStreaming(t, svc, b64BackfillAccount(false), &OpenAIImagesRequest{}, body, nil)
			require.Equal(t, body, rec.Body.String())
			require.Zero(t, upstream.count())
			require.Equal(t, 1, count)
			require.Equal(t, "url", format)
		})
	}
}

func TestOpenAIImagesURLPolicy_HandlerBillingAndHeaders(t *testing.T) {
	body := `{"created":1,"data":[{"url":"https://cdn.example.com/a.png"},{"url":"https://cdn.example.com/b.png"}],"usage":{"input_tokens":3}}`
	upstream := &mediaUpstreamStub{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeInline})
	_, wantCount, wantFormat := runImagesNonStreaming(t, newURLPolicyService(&mediaUpstreamStub{}, ImageURLPolicy{}), b64BackfillAccount(false), &OpenAIImagesRequest{}, body, nil)

	rec, count, format := runImagesNonStreaming(t, svc, b64BackfillAccount(false), &OpenAIImagesRequest{ResponseFormat: "b64_json"}, body, nil)

	require.NotContains(t, rec.Body.String(), "cdn.example.com")
	require.Equal(t, wantCount, count, "改写前后计费图片数一致")
	require.Equal(t, wantFormat, format, "计费 response_format 以上游原始 body 为准")
	require.Empty(t, rec.Header().Get("ETag"))
	require.Empty(t, rec.Header().Get("Content-MD5"))
	require.Equal(t, 2, upstream.count())
}

func TestOpenAIImagesURLPolicy_AsyncMarkerSkipsRewriteAndKeepsBackfill(t *testing.T) {
	body := `{"created":1,"data":[{"url":"https://cdn.example.com/a.png"}]}`
	upstream := &mediaUpstreamStub{}
	storage := &memoryImageStorage{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeRehost, Storage: storage, Prefix: "images/", MaxDownloadBytes: 1 << 20})

	rec, _, _ := runImagesNonStreaming(t, svc, b64BackfillAccount(false), &OpenAIImagesRequest{}, body, WithImageURLRewriteSkipped(context.Background()))
	require.Equal(t, body, rec.Body.String(), "异步子请求由 ImageTaskService.Complete 统一转存")
	require.Zero(t, upstream.count())
	require.Empty(t, storage.saved)

	// 账号开了回填时，异步子请求保持原有回填行为（不受 url_mode 影响）。
	rec, _, _ = runImagesNonStreaming(t, svc, b64BackfillAccount(true), &OpenAIImagesRequest{}, body, WithImageURLRewriteSkipped(context.Background()))
	require.Equal(t, "https://cdn.example.com/a.png", gjson.Get(rec.Body.String(), "data.0.url").String())
	require.NotEmpty(t, gjson.Get(rec.Body.String(), "data.0.b64_json").String())
	require.Empty(t, storage.saved)
}

func TestOpenAIImagesURLPolicy_InlineSkipsLegacyBackfill(t *testing.T) {
	upstream := &mediaUpstreamStub{}
	svc := newURLPolicyService(upstream, ImageURLPolicy{Mode: ImageURLModeInline})
	rec, _, _ := runImagesNonStreaming(t, svc, b64BackfillAccount(true), &OpenAIImagesRequest{},
		`{"data":[{"url":"https://cdn.example.com/a.png"}]}`, nil)
	require.Equal(t, 1, upstream.count(), "回填与改写不重复下载")
	require.False(t, gjson.Get(rec.Body.String(), "data.0.url").Exists())
	require.Equal(t, base64.StdEncoding.EncodeToString(b64BackfillPNGBytes), gjson.Get(rec.Body.String(), "data.0.b64_json").String())
}

func TestOpenAIImagesURLPolicy_StrictRejectsOnlyWhenUpstreamURLWouldLeak(t *testing.T) {
	pngB64 := base64.StdEncoding.EncodeToString(b64BackfillPNGBytes)
	tests := []struct {
		name       string
		policy     ImageURLPolicy
		body       string
		handler    func(*http.Request) (*http.Response, error)
		wantReject bool
	}{
		{
			name:   "下载失败 → 拒绝",
			policy: ImageURLPolicy{Mode: ImageURLModeInline, Strict: true},
			body:   `{"data":[{"url":"https://cdn.example.com/a.png"}]}`,
			handler: func(*http.Request) (*http.Response, error) {
				return b64BackfillImageResponse(http.StatusNotFound, "text/plain", []byte("gone")), nil
			},
			wantReject: true,
		},
		{
			name:       "私网地址不发请求也算暴露 → 拒绝",
			policy:     ImageURLPolicy{Mode: ImageURLModeInline, Strict: true},
			body:       `{"data":[{"url":"http://10.0.0.8/a.png"}]}`,
			wantReject: true,
		},
		{
			name:   "多图中一张失败 → 整体拒绝",
			policy: ImageURLPolicy{Mode: ImageURLModeInline, Strict: true},
			body:   `{"data":[{"url":"https://cdn.example.com/ok.png"},{"url":"https://cdn.example.com/bad.png"}]}`,
			handler: func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/bad.png" {
					return nil, context.DeadlineExceeded
				}
				return b64BackfillImageResponse(http.StatusOK, "image/png", b64BackfillPNGBytes), nil
			},
			wantReject: true,
		},
		{
			name:   "rehost 上传失败退化为 data URL 不暴露 → 放行",
			policy: ImageURLPolicy{Mode: ImageURLModeRehost, Storage: &memoryImageStorage{err: errors.New("s3 down")}, Prefix: "images/", Strict: true},
			body:   `{"data":[{"url":"https://cdn.example.com/a.png"}]}`,
		},
		{
			name:   "全部成功 → 放行",
			policy: ImageURLPolicy{Mode: ImageURLModeInline, Strict: true},
			body:   `{"data":[{"url":"https://cdn.example.com/a.png"}]}`,
		},
		{
			name:   "上游只给 b64 → 放行",
			policy: ImageURLPolicy{Mode: ImageURLModeInline, Strict: true},
			body:   `{"data":[{"b64_json":"` + pngB64 + `"}]}`,
		},
		{
			name:   "未开严格：下载失败保留上游 url",
			policy: ImageURLPolicy{Mode: ImageURLModeInline},
			body:   `{"data":[{"url":"https://cdn.example.com/a.png"}]}`,
			handler: func(*http.Request) (*http.Response, error) {
				return b64BackfillImageResponse(http.StatusNotFound, "text/plain", []byte("gone")), nil
			},
		},
		{
			name:   "passthrough 下严格开关不生效",
			policy: ImageURLPolicy{Mode: ImageURLModePassthrough, Strict: true},
			body:   `{"data":[{"url":"https://cdn.example.com/a.png"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newURLPolicyService(&mediaUpstreamStub{handler: tt.handler}, tt.policy)
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tt.body))}
			usage, count, sizes, format, err := svc.handleOpenAIImagesNonStreamingResponse(context.Background(), resp, c, b64BackfillAccount(false), "https://relay.example.com/v1/images/generations", &OpenAIImagesRequest{})

			if !tt.wantReject {
				require.NoError(t, err)
				require.NotZero(t, count)
				return
			}
			require.ErrorIs(t, err, ErrOpenAIImagesURLRewriteIncomplete)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "不能触发换号：上游已出图")
			require.Empty(t, rec.Body.String(), "交给 handler 写 502，service 不写响应体")
			require.False(t, c.Writer.Written())
			require.Zero(t, count, "客户端没拿到图不计费")
			require.Nil(t, sizes)
			require.Empty(t, format)
			require.Equal(t, OpenAIUsage{}, usage)
		})
	}
}
