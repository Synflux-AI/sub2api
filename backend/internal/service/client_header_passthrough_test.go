//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newClientHeaderPassthroughGinContext(t *testing.T, headers http.Header) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader("{}"))
	for k, vs := range headers {
		for _, v := range vs {
			c.Request.Header.Add(k, v)
		}
	}
	return c
}

func newClientHeaderPassthroughOutbound(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://upstream.example.com/v1", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", "account-key")
	req.Header.Set("Authorization", "Bearer account-token")
	return req
}

func TestNormalizeClientHeaderPassthroughAllowlist(t *testing.T) {
	got, err := NormalizeClientHeaderPassthroughAllowlist([]string{" X-Session-Id ", "x-session-id", "", "Session_Id", "X-Custom"})
	require.NoError(t, err)
	require.Equal(t, []string{"x-session-id", "session_id", "x-custom"}, got)

	empty, err := NormalizeClientHeaderPassthroughAllowlist(nil)
	require.NoError(t, err)
	require.NotNil(t, empty, "空白名单应序列化为 [] 而不是 null")
	require.Empty(t, empty)

	for _, protected := range []string{
		"Host", "Content-Length", "Connection", "Transfer-Encoding", "TE", "Upgrade", "Keep-Alive",
		"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie", "Proxy-Authorization",
		"Content-Type", "Accept-Encoding", "Content-Encoding", "X-Trace-Id", "Sec-WebSocket-Key",
	} {
		_, err := NormalizeClientHeaderPassthroughAllowlist([]string{"x-ok", protected})
		require.Error(t, err, "受保护头 %s 必须在保存时被拒绝", protected)
	}

	for _, invalid := range []string{"bad header", "bad:header", strings.Repeat("a", maxClientHeaderPassthroughNameLength+1)} {
		_, err := NormalizeClientHeaderPassthroughAllowlist([]string{invalid})
		require.Error(t, err, "非法头名 %q 必须被拒绝", invalid)
	}

	tooMany := make([]string, 0, maxClientHeaderPassthroughEntries+1)
	for i := 0; i <= maxClientHeaderPassthroughEntries; i++ {
		tooMany = append(tooMany, "x-h-"+strings.Repeat("a", i+1))
	}
	_, err = NormalizeClientHeaderPassthroughAllowlist(tooMany)
	require.Error(t, err)
}

func TestParseClientHeaderPassthroughAllowlistSetting_SkipsInvalidStoredEntries(t *testing.T) {
	require.Equal(t, []string{}, parseClientHeaderPassthroughAllowlistSetting(""))
	require.Equal(t, []string{}, parseClientHeaderPassthroughAllowlistSetting("not-json"))
	require.Equal(t, []string{"x-session-id", "x-custom"},
		parseClientHeaderPassthroughAllowlistSetting(`["X-Session-Id","authorization","bad header","x-session-id","x-custom"]`))
}

func TestNormalizeClientHeaderPassthroughBlocklistExtra(t *testing.T) {
	extra := map[string]any{AccountExtraClientHeaderPassthroughBlocklist: []any{" X-Session-Id ", "x-session-id", "Authorization", ""}}
	require.NoError(t, NormalizeClientHeaderPassthroughBlocklistExtra(extra))
	require.Equal(t, []string{"x-session-id", "authorization"}, extra[AccountExtraClientHeaderPassthroughBlocklist])

	cleared := map[string]any{AccountExtraClientHeaderPassthroughBlocklist: []any{" "}}
	require.NoError(t, NormalizeClientHeaderPassthroughBlocklistExtra(cleared))
	require.NotContains(t, cleared, AccountExtraClientHeaderPassthroughBlocklist, "规范化后为空应移除该键")

	require.NoError(t, NormalizeClientHeaderPassthroughBlocklistExtra(nil))
	require.NoError(t, NormalizeClientHeaderPassthroughBlocklistExtra(map[string]any{"other": 1}))

	require.Error(t, NormalizeClientHeaderPassthroughBlocklistExtra(map[string]any{AccountExtraClientHeaderPassthroughBlocklist: "x-session-id"}))
	require.Error(t, NormalizeClientHeaderPassthroughBlocklistExtra(map[string]any{AccountExtraClientHeaderPassthroughBlocklist: []any{1}}))
	require.Error(t, NormalizeClientHeaderPassthroughBlocklistExtra(map[string]any{AccountExtraClientHeaderPassthroughBlocklist: []any{"bad header"}}))
}

func TestApplyClientHeaderPassthrough_CopiesAllowlistedHeaders(t *testing.T) {
	c := newClientHeaderPassthroughGinContext(t, http.Header{
		"X-Session-Id": {"sess-1"},
		"X-Multi":      {"a", "b"},
		"X-Unlisted":   {"nope"},
	})
	req := newClientHeaderPassthroughOutbound(t)

	applyClientHeaderPassthrough(c, req, &Account{ID: 1}, []string{"x-session-id", "x-multi", "x-absent"})

	require.Equal(t, "sess-1", req.Header.Get("X-Session-Id"))
	require.Equal(t, []string{"a", "b"}, req.Header.Values("X-Multi"))
	require.Empty(t, req.Header.Values("X-Unlisted"), "不在白名单的头不能透传")
	require.Empty(t, req.Header.Values("X-Absent"), "客户端没带就不写")
}

func TestApplyClientHeaderPassthrough_AccountBlocklistRemoves(t *testing.T) {
	c := newClientHeaderPassthroughGinContext(t, http.Header{"X-Session-Id": {"sess-1"}, "X-Custom": {"v"}})
	req := newClientHeaderPassthroughOutbound(t)
	account := &Account{ID: 1, Extra: map[string]any{AccountExtraClientHeaderPassthroughBlocklist: []any{"X-Session-Id"}}}

	applyClientHeaderPassthrough(c, req, account, []string{"x-session-id", "x-custom"})

	require.Empty(t, req.Header.Values("X-Session-Id"))
	require.Equal(t, "v", req.Header.Get("X-Custom"))
}

func TestApplyClientHeaderPassthrough_NeverOverridesGatewayHeaders(t *testing.T) {
	c := newClientHeaderPassthroughGinContext(t, http.Header{
		"Authorization":  {"Bearer client-token"},
		"X-Goog-Api-Key": {"client-key"},
		"Content-Type":   {"text/plain"},
		"User-Agent":     {"client-ua"},
	})
	req := newClientHeaderPassthroughOutbound(t)
	req.Header.Set("User-Agent", "gateway-ua")
	// 模拟写死白名单路径上以 wire casing 写入的小写 key
	req.Header["anthropic-beta"] = []string{"gateway-beta"}
	c.Request.Header.Set("Anthropic-Beta", "client-beta")

	// 绕过保存校验（例如旧数据）把受保护头放进白名单，也不能覆盖认证头
	applyClientHeaderPassthrough(c, req, &Account{ID: 1}, []string{"authorization", "x-goog-api-key", "content-type", "user-agent", "anthropic-beta"})

	require.Equal(t, []string{"Bearer account-token"}, req.Header.Values("Authorization"))
	require.Equal(t, []string{"account-key"}, req.Header.Values("X-Goog-Api-Key"))
	require.Equal(t, []string{"application/json"}, req.Header.Values("Content-Type"))
	require.Equal(t, []string{"gateway-ua"}, req.Header.Values("User-Agent"))
	require.Equal(t, []string{"gateway-beta"}, req.Header["anthropic-beta"])
	require.NotContains(t, req.Header, "Anthropic-Beta", "同一个头不能以两种拼写同时出现")
}

func TestApplyClientHeaderPassthrough_DropsUnsafeValues(t *testing.T) {
	c := newClientHeaderPassthroughGinContext(t, nil)
	// 直接写 map 绕过 Header.Add 的规整，模拟中间件或测试注入的脏值
	c.Request.Header["X-Crlf"] = []string{"a\r\nInjected: 1"}
	c.Request.Header["X-Nul"] = []string{"a\x00b"}
	c.Request.Header["X-Tab"] = []string{"a\tb"}
	c.Request.Header["X-Bad-Utf8"] = []string{"a\xffb"}
	c.Request.Header["X-Too-Long"] = []string{strings.Repeat("a", maxClientHeaderPassthroughValueLength+1)}
	c.Request.Header["X-Mixed"] = []string{"good", "bad\n"}
	c.Request.Header["X-Ok"] = []string{"会话-1"}
	req := newClientHeaderPassthroughOutbound(t)

	applyClientHeaderPassthrough(c, req, &Account{ID: 1},
		[]string{"x-crlf", "x-nul", "x-tab", "x-bad-utf8", "x-too-long", "x-mixed", "x-ok"})

	for _, name := range []string{"X-Crlf", "X-Nul", "X-Tab", "X-Bad-Utf8", "X-Too-Long", "X-Mixed"} {
		require.Empty(t, req.Header.Values(name), "%s 含非法值，应整头丢弃", name)
	}
	require.Equal(t, "会话-1", req.Header.Get("X-Ok"))
	require.Empty(t, req.Header.Values("Injected"))
}

func TestApplyClientHeaderPassthrough_EmptyAllowlistIsNoop(t *testing.T) {
	c := newClientHeaderPassthroughGinContext(t, http.Header{"X-Session-Id": {"sess-1"}})
	req := newClientHeaderPassthroughOutbound(t)
	before := req.Header.Clone()

	applyClientHeaderPassthrough(c, req, &Account{ID: 1}, nil)
	applyClientHeaderPassthrough(c, req, &Account{ID: 1}, []string{})
	applyClientHeaderPassthrough(nil, req, &Account{ID: 1}, []string{"x-session-id"})

	require.Equal(t, before, req.Header)
}

// ---------------------------------------------------------------------------
// Gemini 原生 / compat 路径回归：配置 X-Session-Id 后上游收到同一个值，
// 未配置时出站请求头与现状一致，重试与换号按当前账号重新计算。
// ---------------------------------------------------------------------------

const clientHeaderPassthroughGeminiBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

// useClientHeaderPassthroughAllowlist 让全局白名单生效，并在测试结束后恢复进程内缓存。
func useClientHeaderPassthroughAllowlist(t *testing.T, names ...string) *SettingService {
	t.Helper()
	raw, err := json.Marshal(names)
	require.NoError(t, err)
	repo := &gatewayTTLSettingRepo{data: map[string]string{SettingKeyClientHeaderPassthroughAllowlist: string(raw)}}
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
	t.Cleanup(func() { gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{}) })
	return NewSettingService(repo, &config.Config{})
}

// geminiRecordingUpstream 记录每次出站请求的头，每次返回一个新的成功响应。
type geminiRecordingUpstream struct {
	headers []http.Header
	status  []int
}

func (u *geminiRecordingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.headers = append(u.headers, req.Header.Clone())
	status := http.StatusOK
	if i := len(u.headers) - 1; i < len(u.status) {
		status = u.status[i]
	}
	body := geminiVertexAPIKeyTestResponse
	if status != http.StatusOK {
		body = `{"error":{"code":500,"message":"internal","status":"INTERNAL"}}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (u *geminiRecordingUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func clientHeaderPassthroughGeminiAccounts(t *testing.T) map[string]*Account {
	t.Helper()
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	return map[string]*Account{
		"api_key": {
			ID: 901, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1,
			Credentials: map[string]any{"api_key": "ak-test"},
		},
		"oauth_code_assist": {
			ID: 902, Platform: PlatformGemini, Type: AccountTypeOAuth, Concurrency: 1,
			Credentials: map[string]any{"access_token": "oauth-token", "expires_at": future, "project_id": "proj-ca"},
		},
		"oauth_ai_studio": {
			ID: 903, Platform: PlatformGemini, Type: AccountTypeOAuth, Concurrency: 1,
			Credentials: map[string]any{"access_token": "oauth-token", "expires_at": future},
		},
		"vertex_service_account": newVertexTokenTestAccount(t),
	}
}

func requireClientHeaderPassthroughAuth(t *testing.T, name string, h http.Header) {
	t.Helper()
	switch name {
	case "api_key":
		require.Equal(t, []string{"ak-test"}, h.Values("X-Goog-Api-Key"))
		require.Empty(t, h.Values("Authorization"))
	case "oauth_code_assist", "oauth_ai_studio":
		require.Equal(t, []string{"Bearer oauth-token"}, h.Values("Authorization"))
		require.Empty(t, h.Values("X-Goog-Api-Key"))
	case "vertex_service_account":
		require.Equal(t, []string{"Bearer sa-token"}, h.Values("Authorization"))
		require.Empty(t, h.Values("X-Goog-Api-Key"))
	}
}

func newClientHeaderPassthroughGeminiService(settingService *SettingService) (*GeminiMessagesCompatService, *geminiRecordingUpstream) {
	upstream := &geminiRecordingUpstream{}
	return &GeminiMessagesCompatService{
		httpUpstream:   upstream,
		tokenProvider:  NewGeminiTokenProvider(nil, nil, nil),
		settingService: settingService,
		cfg:            &config.Config{Gateway: config.GatewayConfig{MaxSameAccountRetriesGemini: geminiDefaultSameAccountRetries}},
	}, upstream
}

func newClientHeaderPassthroughGeminiContext(t *testing.T) *gin.Context {
	t.Helper()
	return newClientHeaderPassthroughGinContext(t, http.Header{
		"X-Session-Id":   {"sess-abc"},
		"Authorization":  {"Bearer client-token"},
		"X-Goog-Api-Key": {"client-key"},
	})
}

func TestGeminiForwardNative_ClientHeaderPassthroughPerAccountType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useVertexTokenEndpoint(t, http.StatusOK, `{"access_token":"sa-token","expires_in":3600,"token_type":"Bearer"}`)
	settingService := useClientHeaderPassthroughAllowlist(t, "X-Session-Id")

	for name, account := range clientHeaderPassthroughGeminiAccounts(t) {
		t.Run(name, func(t *testing.T) {
			svc, upstream := newClientHeaderPassthroughGeminiService(settingService)
			c := newClientHeaderPassthroughGeminiContext(t)

			result, err := svc.ForwardNative(context.Background(), c, account, "gemini-2.5-flash", "generateContent", false, []byte(clientHeaderPassthroughGeminiBody))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.headers, 1)
			require.Equal(t, []string{"sess-abc"}, upstream.headers[0].Values("X-Session-Id"))
			requireClientHeaderPassthroughAuth(t, name, upstream.headers[0])
		})
	}
}

func TestGeminiForwardNative_ClientHeaderPassthroughUnconfiguredKeepsHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useVertexTokenEndpoint(t, http.StatusOK, `{"access_token":"sa-token","expires_in":3600,"token_type":"Bearer"}`)
	configured := useClientHeaderPassthroughAllowlist(t) // 白名单为空

	for name, account := range clientHeaderPassthroughGeminiAccounts(t) {
		t.Run(name, func(t *testing.T) {
			withSetting, withUpstream := newClientHeaderPassthroughGeminiService(configured)
			_, err := withSetting.ForwardNative(context.Background(), newClientHeaderPassthroughGeminiContext(t), account, "gemini-2.5-flash", "generateContent", false, []byte(clientHeaderPassthroughGeminiBody))
			require.NoError(t, err)

			// 基线：不注入 settingService，即接入前的行为
			baseline, baselineUpstream := newClientHeaderPassthroughGeminiService(nil)
			_, err = baseline.ForwardNative(context.Background(), newClientHeaderPassthroughGeminiContext(t), account, "gemini-2.5-flash", "generateContent", false, []byte(clientHeaderPassthroughGeminiBody))
			require.NoError(t, err)

			require.Len(t, withUpstream.headers, 1)
			require.Len(t, baselineUpstream.headers, 1)
			require.Equal(t, baselineUpstream.headers[0], withUpstream.headers[0])
			require.Empty(t, withUpstream.headers[0].Values("X-Session-Id"))
		})
	}
}

func TestGeminiForwardNative_ClientHeaderPassthroughReappliedOnRetryAndAccountSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settingService := useClientHeaderPassthroughAllowlist(t, "x-session-id", "x-custom")
	svc, upstream := newClientHeaderPassthroughGeminiService(settingService)
	upstream.status = []int{http.StatusInternalServerError, http.StatusOK}
	c := newClientHeaderPassthroughGinContext(t, http.Header{"X-Session-Id": {"sess-abc"}, "X-Custom": {"v1"}})

	first := &Account{ID: 911, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "ak-1"}}
	_, err := svc.ForwardNative(context.Background(), c, first, "gemini-2.5-flash", "generateContent", false, []byte(clientHeaderPassthroughGeminiBody))
	require.NoError(t, err)
	require.Len(t, upstream.headers, 2, "500 应在同一账号上重试一次")
	for i, h := range upstream.headers {
		require.Equal(t, []string{"sess-abc"}, h.Values("X-Session-Id"), "第 %d 次尝试", i+1)
		require.Equal(t, []string{"v1"}, h.Values("X-Custom"), "第 %d 次尝试", i+1)
	}

	// 换号：同一个入站请求换到禁止透传 X-Session-Id 的账号
	second := &Account{
		ID: 912, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "ak-2"},
		Extra:       map[string]any{AccountExtraClientHeaderPassthroughBlocklist: []any{"x-session-id"}},
	}
	_, err = svc.ForwardNative(context.Background(), c, second, "gemini-2.5-flash", "generateContent", false, []byte(clientHeaderPassthroughGeminiBody))
	require.NoError(t, err)
	last := upstream.headers[len(upstream.headers)-1]
	require.Empty(t, last.Values("X-Session-Id"), "换号后应按新账号的黑名单重新计算")
	require.Equal(t, []string{"v1"}, last.Values("X-Custom"))
	require.Equal(t, []string{"ak-2"}, last.Values("X-Goog-Api-Key"))
}

func TestGeminiCompatPaths_ClientHeaderPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settingService := useClientHeaderPassthroughAllowlist(t, "x-session-id")
	account := &Account{ID: 921, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "ak-test"}}

	t.Run("messages", func(t *testing.T) {
		svc, upstream := newClientHeaderPassthroughGeminiService(settingService)
		c := newClientHeaderPassthroughGeminiContext(t)
		body := []byte(`{"model":"gemini-2.5-flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
		_, err := svc.Forward(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Len(t, upstream.headers, 1)
		require.Equal(t, []string{"sess-abc"}, upstream.headers[0].Values("X-Session-Id"))
		requireClientHeaderPassthroughAuth(t, "api_key", upstream.headers[0])
	})

	t.Run("chat_completions", func(t *testing.T) {
		svc, upstream := newClientHeaderPassthroughGeminiService(settingService)
		c := newClientHeaderPassthroughGeminiContext(t)
		body := []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`)
		_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Len(t, upstream.headers, 1)
		require.Equal(t, []string{"sess-abc"}, upstream.headers[0].Values("X-Session-Id"))
		requireClientHeaderPassthroughAuth(t, "api_key", upstream.headers[0])
	})
}

func TestGeminiForwardNative_ClientHeaderPassthroughBeforeTraceAndCustomHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settingService := useClientHeaderPassthroughAllowlist(t, "x-custom")
	svc, upstream := newClientHeaderPassthroughGeminiService(settingService)
	c := newClientHeaderPassthroughGinContext(t, http.Header{"X-Custom": {"client"}})
	account := &Account{
		ID: 931, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials:          map[string]any{"api_key": "ak-test"},
		Extra:                map[string]any{"trace_id_passthrough": true},
		CustomHeadersEnabled: true,
		CustomHeaders:        map[string]string{"X-Custom": "account"},
	}
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "0123456789abcdef0123456789abcdef")

	_, err := svc.ForwardNative(ctx, c, account, "gemini-2.5-flash", "generateContent", false, []byte(clientHeaderPassthroughGeminiBody))
	require.NoError(t, err)
	require.Len(t, upstream.headers, 1)
	require.Equal(t, []string{"account"}, upstream.headers[0].Values("X-Custom"), "账号自定义头必须最终生效")
	require.NotEmpty(t, upstream.headers[0].Get("X-Trace-Id"))
}

func TestSettingService_UpdateSettings_ClientHeaderPassthroughAllowlist(t *testing.T) {
	t.Cleanup(func() { gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{}) })

	t.Run("rejects protected header", func(t *testing.T) {
		repo := &settingUpdateRepoStub{}
		svc := NewSettingService(repo, &config.Config{})
		err := svc.UpdateSettings(context.Background(), &SystemSettings{ClientHeaderPassthroughAllowlist: []string{"x-session-id", "Authorization"}})
		require.Error(t, err)
		require.Nil(t, repo.updates, "校验失败不能落库")
	})

	t.Run("normalizes and persists", func(t *testing.T) {
		repo := &settingUpdateRepoStub{}
		svc := NewSettingService(repo, &config.Config{})
		require.NoError(t, svc.UpdateSettings(context.Background(), &SystemSettings{ClientHeaderPassthroughAllowlist: []string{" X-Session-Id ", "x-session-id"}}))
		require.Equal(t, `["x-session-id"]`, repo.updates[SettingKeyClientHeaderPassthroughAllowlist])
		require.Equal(t, []string{"x-session-id"}, svc.GetClientHeaderPassthroughAllowlist(context.Background()))
	})

	t.Run("defaults to empty", func(t *testing.T) {
		repo := &settingUpdateRepoStub{}
		svc := NewSettingService(repo, &config.Config{})
		require.NoError(t, svc.UpdateSettings(context.Background(), &SystemSettings{}))
		require.Equal(t, `[]`, repo.updates[SettingKeyClientHeaderPassthroughAllowlist])
	})
}
