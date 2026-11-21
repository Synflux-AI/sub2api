//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func endUserTestAccount(extra map[string]any) *Account {
	return &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: extra}
}

func TestResolveEndUserID(t *testing.T) {
	admin := &User{ID: 1, Role: RoleAdmin}
	member := &User{ID: 2, Role: RoleUser}

	t.Run("无 API key 时不产生 ID", func(t *testing.T) {
		require.Empty(t, ResolveEndUserID(nil, "345"))
	})
	t.Run("入口实例取当前 API key ID", func(t *testing.T) {
		require.Equal(t, "12", ResolveEndUserID(&APIKey{ID: 12, User: member}, ""))
	})
	t.Run("非管理员 key 携带的入站值被忽略", func(t *testing.T) {
		require.Equal(t, "12", ResolveEndUserID(&APIKey{ID: 12, User: member}, "345"))
	})
	t.Run("管理员 key 携带合法入站值时原样沿用", func(t *testing.T) {
		require.Equal(t, "a:345", ResolveEndUserID(&APIKey{ID: 12, User: admin}, " a:345 "))
	})
	t.Run("管理员 key 携带非法入站值时回退到 key ID", func(t *testing.T) {
		for _, inbound := range []string{"", "has space", "alice@example.com", strings.Repeat("x", 129)} {
			require.Equal(t, "12", ResolveEndUserID(&APIKey{ID: 12, User: admin}, inbound), inbound)
		}
	})
}

func TestInjectEndUserHeader(t *testing.T) {
	ctx := WithEndUserID(context.Background(), "345")

	t.Run("未开启：不写出终端用户头，且剥离内部转发头", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example.com", nil)
		req.Header["x-sub2api-end-user"] = []string{"forged"}
		req.Header.Set(EndUserForwardHeader, "forged")
		injectEndUserHeader(ctx, req, endUserTestAccount(nil))
		require.Empty(t, req.Header)
	})

	t.Run("开启：默认头名写出 ID，并覆盖任意大小写的同名头", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example.com", nil)
		req.Header["x-end-user"] = []string{"client-value"}
		req.Header.Set(EndUserForwardHeader, "inbound")
		injectEndUserHeader(ctx, req, endUserTestAccount(map[string]any{
			AccountExtraEndUserHeaderEnabled: true,
		}))
		require.Equal(t, http.Header{"X-End-User": {"345"}}, req.Header)
	})

	t.Run("开启：自定义头名与前缀", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example.com", nil)
		injectEndUserHeader(ctx, req, endUserTestAccount(map[string]any{
			AccountExtraEndUserHeaderEnabled: true,
			AccountExtraEndUserHeaderName:    EndUserForwardHeader,
			AccountExtraEndUserHeaderPrefix:  "a:",
		}))
		require.Equal(t, http.Header{"X-Sub2api-End-User": {"a:345"}}, req.Header)
	})

	t.Run("开启但请求无终端用户 ID：不写出", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example.com", nil)
		injectEndUserHeader(context.Background(), req, endUserTestAccount(map[string]any{
			AccountExtraEndUserHeaderEnabled: true,
		}))
		require.Empty(t, req.Header)
	})

	t.Run("拼接前缀后超长：不写出", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example.com", nil)
		longCtx := WithEndUserID(context.Background(), strings.Repeat("9", 128))
		injectEndUserHeader(longCtx, req, endUserTestAccount(map[string]any{
			AccountExtraEndUserHeaderEnabled: true,
			AccountExtraEndUserHeaderPrefix:  "a:",
		}))
		require.Empty(t, req.Header)
	})

	t.Run("落库的非法头名回退到默认头名", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example.com", nil)
		injectEndUserHeader(ctx, req, endUserTestAccount(map[string]any{
			AccountExtraEndUserHeaderEnabled: true,
			AccountExtraEndUserHeaderName:    "Authorization",
		}))
		require.Equal(t, http.Header{"X-End-User": {"345"}}, req.Header)
	})

	t.Run("nil 请求与 nil 账号不 panic", func(t *testing.T) {
		injectEndUserHeader(ctx, nil, nil)
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example.com", nil)
		injectEndUserHeader(ctx, req, nil)
		require.Empty(t, req.Header)
	})
}

func TestNormalizeEndUserHeaderExtra(t *testing.T) {
	t.Run("合法配置被规范化", func(t *testing.T) {
		extra := map[string]any{
			AccountExtraEndUserHeaderEnabled: true,
			AccountExtraEndUserHeaderName:    "  X-End-User ",
			AccountExtraEndUserHeaderPrefix:  " a: ",
		}
		require.NoError(t, NormalizeEndUserHeaderExtra(extra))
		require.Equal(t, "X-End-User", extra[AccountExtraEndUserHeaderName])
		require.Equal(t, "a:", extra[AccountExtraEndUserHeaderPrefix])
	})
	t.Run("内部转发头可作为出站头名", func(t *testing.T) {
		extra := map[string]any{AccountExtraEndUserHeaderName: "x-sub2api-end-user"}
		require.NoError(t, NormalizeEndUserHeaderExtra(extra))
	})
	t.Run("空值被移除", func(t *testing.T) {
		extra := map[string]any{AccountExtraEndUserHeaderName: " ", AccountExtraEndUserHeaderPrefix: ""}
		require.NoError(t, NormalizeEndUserHeaderExtra(extra))
		require.NotContains(t, extra, AccountExtraEndUserHeaderName)
		require.NotContains(t, extra, AccountExtraEndUserHeaderPrefix)
	})
	t.Run("非法配置被拒绝", func(t *testing.T) {
		for _, extra := range []map[string]any{
			{AccountExtraEndUserHeaderEnabled: "true"},
			{AccountExtraEndUserHeaderName: "Authorization"},
			{AccountExtraEndUserHeaderName: "bad header"},
			{AccountExtraEndUserHeaderName: 1},
			{AccountExtraEndUserHeaderPrefix: "a b"},
			{AccountExtraEndUserHeaderPrefix: strings.Repeat("a", 33)},
		} {
			require.Error(t, NormalizeEndUserHeaderExtra(extra), "%v", extra)
		}
	})
	t.Run("nil 与未携带字段为 no-op", func(t *testing.T) {
		require.NoError(t, NormalizeEndUserHeaderExtra(nil))
		require.NoError(t, NormalizeEndUserHeaderExtra(map[string]any{"other": 1}))
	})
}

func TestEndUserForwardHeaderIsProtected(t *testing.T) {
	require.True(t, IsClientHeaderPassthroughBlockedName("X-Sub2api-End-User"))
	require.True(t, isHeaderOverrideBlockedName("x-sub2api-end-user"))
}

func TestOpenAIWSHandshakeCompatibilitySeparatesEndUsers(t *testing.T) {
	enabled := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{
		AccountExtraEndUserHeaderEnabled: true,
	}}
	headersFor := func(account *Account, id string) http.Header {
		h := make(http.Header)
		applyEndUserHeader(WithEndUserID(context.Background(), id), h, account)
		return h
	}

	require.NotEqual(t,
		normalizeOpenAIWSHandshakeCompatibility(enabled, headersFor(enabled, "1")),
		normalizeOpenAIWSHandshakeCompatibility(enabled, headersFor(enabled, "2")),
	)
	require.Equal(t,
		normalizeOpenAIWSHandshakeCompatibility(enabled, headersFor(enabled, "1")),
		normalizeOpenAIWSHandshakeCompatibility(enabled, headersFor(enabled, "1")),
	)

	disabled := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t,
		normalizeOpenAIWSHandshakeCompatibility(disabled, headersFor(disabled, "1")),
		normalizeOpenAIWSHandshakeCompatibility(disabled, headersFor(disabled, "2")),
	)
}

func TestBuildCountTokensRequest_InjectsEndUserHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	// 入站的内部转发头与客户端自带的出站同名头都不得到达上游。
	c.Request.Header.Set(EndUserForwardHeader, "inbound")
	c.Request.Header.Set(DefaultEndUserHeaderName, "client-supplied")

	account := &Account{
		ID: 4213, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-ant-xxx"},
		Status:      StatusActive, Schedulable: true,
		Extra: map[string]any{AccountExtraEndUserHeaderEnabled: true},
	}
	body := []byte(`{"model":"claude-haiku-4-5","messages":[]}`)
	ctx := WithEndUserID(context.Background(), "345")
	svc := &GatewayService{cfg: &config.Config{}}

	req, _, err := svc.buildCountTokensRequest(ctx, c, account, body, "sk-ant-xxx", "apikey", "claude-haiku-4-5", false)
	require.NoError(t, err)
	require.Equal(t, []string{"345"}, req.Header.Values(DefaultEndUserHeaderName))
	require.Empty(t, req.Header.Values(EndUserForwardHeader))

	account.Extra = nil
	reqOff, _, err := svc.buildCountTokensRequest(ctx, c, account, body, "sk-ant-xxx", "apikey", "claude-haiku-4-5", false)
	require.NoError(t, err)
	require.Empty(t, reqOff.Header.Values(DefaultEndUserHeaderName))
	require.Empty(t, reqOff.Header.Values(EndUserForwardHeader))
}
