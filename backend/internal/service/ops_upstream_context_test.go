package service

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSafeUpstreamURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"strips query", "https://api.anthropic.com/v1/messages?beta=true", "https://api.anthropic.com/v1/messages"},
		{"strips fragment", "https://api.openai.com/v1/responses#frag", "https://api.openai.com/v1/responses"},
		{"strips both", "https://host/path?token=secret#x", "https://host/path"},
		{"no query or fragment", "https://host/path", "https://host/path"},
		{"empty string", "", ""},
		{"whitespace only", "  ", ""},
		{"query before fragment", "https://h/p?a=1#f", "https://h/p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, safeUpstreamURL(tt.input))
		})
	}
}

func TestOpsUpstreamErrorEventKeepsExplicitProxySnapshotPerAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	oldProxyID := int64(10060)
	oldProxy := &Proxy{
		ID:       oldProxyID,
		Name:     "wldsg82-ipv6-10060",
		Protocol: "socks5",
		Host:     "old-proxy.example",
		Username: "proxy-user",
		Password: "proxy-secret",
	}
	oldAccount := &Account{
		ID:       11,
		Name:     "old-account",
		Platform: PlatformOpenAI,
		ProxyID:  &oldProxyID,
		Proxy:    oldProxy,
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		AccountID: oldAccount.ID,
		ProxyID:   opsUpstreamProxyID(oldAccount),
		ProxyName: opsUpstreamProxyName(oldAccount),
		Kind:      "retry",
	})

	newProxyID := int64(8001)
	newAccount := &Account{
		ID:       12,
		Name:     "new-account",
		Platform: PlatformOpenAI,
		ProxyID:  &newProxyID,
		Proxy:    &Proxy{ID: newProxyID, Name: "oxylabs-uk-8001"},
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		AccountID: newAccount.ID,
		ProxyID:   opsUpstreamProxyID(newAccount),
		ProxyName: opsUpstreamProxyName(newAccount),
		Kind:      "failover",
	})

	// Mutating the selected account after both attempts must not rewrite history.
	oldProxy.Name = "mutated-current-proxy"
	oldAccount.ProxyID = &newProxyID
	oldAccount.Proxy = newAccount.Proxy

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)

	require.Equal(t, int64(10060), *events[0].ProxyID)
	require.Equal(t, "wldsg82-ipv6-10060", events[0].ProxyName)

	require.Equal(t, int64(8001), *events[1].ProxyID)
	require.Equal(t, "oxylabs-uk-8001", events[1].ProxyName)

	encoded := marshalOpsUpstreamErrors(events)
	require.NotNil(t, encoded)
	require.NotContains(t, *encoded, "old-proxy.example")
	require.NotContains(t, *encoded, "proxy-user")
	require.NotContains(t, *encoded, "proxy-secret")
}

func TestOpsUpstreamProxyFieldAccessors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	boundID := int64(7)
	backupID := int64(8)
	tests := []struct {
		name     string
		account  *Account
		wantID   *int64
		wantName string
	}{
		{
			name:     "actual proxy object is authoritative",
			account:  &Account{ProxyID: &boundID, Proxy: &Proxy{ID: backupID, Name: "backup-uk"}},
			wantID:   &backupID,
			wantName: "backup-uk",
		},
		{
			name:     "proxy object without binding id is not a configured route",
			account:  &Account{Proxy: &Proxy{ID: backupID, Name: "hydrated-proxy"}},
			wantName: opsProxyNameDirect,
		},
		{
			name:     "hydrated proxy without durable id is unknown, never id=null plus a real name",
			account:  &Account{ProxyID: &boundID, Proxy: &Proxy{Name: "transient-proxy"}},
			wantName: opsProxyNameUnknown,
		},
		{
			name:     "blank proxy name gets the unnamed placeholder",
			account:  &Account{ProxyID: &boundID, Proxy: &Proxy{ID: boundID, Name: "   "}},
			wantID:   &boundID,
			wantName: opsProxyNameUnnamed,
		},
		{
			name:     "account has no proxy",
			account:  &Account{},
			wantName: opsProxyNameDirect,
		},
		{
			name:     "configured proxy was not hydrated and transport is direct",
			account:  &Account{ProxyID: &backupID},
			wantName: opsProxyNameDirect,
		},
		{
			name:     "missing attempt account",
			account:  nil,
			wantName: opsProxyNameUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:   opsUpstreamProxyID(tt.account),
				ProxyName: opsUpstreamProxyName(tt.account),
				Kind:      "request_error",
			})
			raw, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			events, ok := raw.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1)
			event := events[0]
			if tt.wantID == nil {
				require.Nil(t, event.ProxyID)
			} else {
				require.NotNil(t, event.ProxyID)
				require.Equal(t, *tt.wantID, *event.ProxyID)
			}
			require.Equal(t, tt.wantName, event.ProxyName)
		})
	}
}

func TestOpsUpstreamErrorEventRetryKeepsExplicitAttemptProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	proxyID := int64(10060)
	account := &Account{ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Name: "retry-proxy"}}

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:   opsUpstreamProxyID(account),
		ProxyName: opsUpstreamProxyName(account),
		Kind:      "retry",
	})
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:   opsUpstreamProxyID(account),
		ProxyName: opsUpstreamProxyName(account),
		Kind:      "retry_exhausted",
	})

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)
	for _, event := range events {
		require.NotNil(t, event.ProxyID)
		require.Equal(t, proxyID, *event.ProxyID)
		require.Equal(t, "retry-proxy", event.ProxyName)
	}
}

func TestNormalizeOpsUpstreamProxyAttributionEnforcesSentinelInvariant(t *testing.T) {
	positive := int64(7)
	zero := int64(0)
	tests := []struct {
		name     string
		in       OpsUpstreamErrorEvent
		wantID   *int64
		wantName string
	}{
		{name: "id with name kept", in: OpsUpstreamErrorEvent{ProxyID: &positive, ProxyName: "eu-1"}, wantID: &positive, wantName: "eu-1"},
		{name: "id without name gets placeholder", in: OpsUpstreamErrorEvent{ProxyID: &positive}, wantID: &positive, wantName: opsProxyNameUnnamed},
		{name: "null id with real name collapses to unknown", in: OpsUpstreamErrorEvent{ProxyName: "eu-1"}, wantName: opsProxyNameUnknown},
		{name: "zero id with real name collapses to unknown", in: OpsUpstreamErrorEvent{ProxyID: &zero, ProxyName: "eu-1"}, wantName: opsProxyNameUnknown},
		{name: "explicit direct preserved", in: OpsUpstreamErrorEvent{ProxyName: opsProxyNameDirect}, wantName: opsProxyNameDirect},
		{name: "empty is unknown", in: OpsUpstreamErrorEvent{}, wantName: opsProxyNameUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := tt.in
			normalizeOpsUpstreamProxyAttribution(&ev)
			if tt.wantID == nil {
				require.Nil(t, ev.ProxyID)
			} else {
				require.NotNil(t, ev.ProxyID)
				require.Equal(t, *tt.wantID, *ev.ProxyID)
			}
			require.Equal(t, tt.wantName, ev.ProxyName)
		})
	}
}

func TestNormalizeOpsUpstreamErrorsJSONPreservesUnknownKeysAndOrder(t *testing.T) {
	// A legacy row written by an older struct version: it carries a key the
	// current struct no longer has, and mixes explicit/legacy attribution.
	raw := `[` +
		`{"at_unix_ms":1,"account_id":42,"upstream_request_body":"legacy-only-key","kind":"http_error"},` +
		`{"at_unix_ms":2,"proxy_id":null,"proxy_name":"direct/no_proxy","kind":"request_error"},` +
		`{"at_unix_ms":3,"proxy_id":9,"proxy_name":"eu-9","kind":"failover"},` +
		`{"at_unix_ms":4,"proxy_id":0,"proxy_name":"stale-name","kind":"retry"},` +
		`{"at_unix_ms":5,"proxy_id":11,"proxy_name":"","kind":"retry"}` +
		`]`

	out, err := normalizeOpsUpstreamErrorsJSON(raw)
	require.NoError(t, err)

	// Legacy-only key and original key order survive.
	require.Contains(t, out, `"upstream_request_body":"legacy-only-key"`)
	require.True(t, strings.Index(out, `"at_unix_ms":1`) < strings.Index(out, `"account_id":42`))

	events, err := ParseOpsUpstreamErrors(out)
	require.NoError(t, err)
	require.Len(t, events, 5)
	require.Nil(t, events[0].ProxyID)
	require.Equal(t, opsProxyNameUnknown, events[0].ProxyName)
	require.Nil(t, events[1].ProxyID)
	require.Equal(t, opsProxyNameDirect, events[1].ProxyName)
	require.NotNil(t, events[2].ProxyID)
	require.Equal(t, int64(9), *events[2].ProxyID)
	require.Equal(t, "eu-9", events[2].ProxyName)
	require.Nil(t, events[3].ProxyID)
	require.Equal(t, opsProxyNameUnknown, events[3].ProxyName)
	require.NotNil(t, events[4].ProxyID)
	require.Equal(t, opsProxyNameUnnamed, events[4].ProxyName)

	// Already-normalized input is returned byte-for-byte.
	again, err := normalizeOpsUpstreamErrorsJSON(out)
	require.NoError(t, err)
	require.Equal(t, out, again)

	_, err = normalizeOpsUpstreamErrorsJSON(`{"not":"an array"}`)
	require.Error(t, err)
	_, err = normalizeOpsUpstreamErrorsJSON(`[not json`)
	require.Error(t, err)
}

func TestParseOpsUpstreamErrorsMarksLegacyProxyAttributionUnknown(t *testing.T) {
	events, err := ParseOpsUpstreamErrors(`[{"account_id":42,"account_name":"legacy","kind":"http_error"}]`)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Nil(t, events[0].ProxyID)
	require.Equal(t, opsProxyNameUnknown, events[0].ProxyName)
}

func TestParseOpsUpstreamErrorsPreservesExplicitDirectAttribution(t *testing.T) {
	events, err := ParseOpsUpstreamErrors(`[{"proxy_id":null,"proxy_name":"direct/no_proxy","kind":"request_error"}]`)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Nil(t, events[0].ProxyID)
	require.Equal(t, opsProxyNameDirect, events[0].ProxyName)
}

func TestOpsServiceGetErrorLogByIDNormalizesLegacyProxyAttribution(t *testing.T) {
	repo := &opsRepoMock{
		GetErrorLogByIDFn: func(context.Context, int64) (*OpsErrorLogDetail, error) {
			return &OpsErrorLogDetail{UpstreamErrors: `[{"account_id":42,"kind":"http_error"}]`}, nil
		},
	}
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	detail, err := svc.GetErrorLogByID(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, detail)
	require.Contains(t, detail.UpstreamErrors, `"proxy_name":"unknown"`)
	require.Contains(t, detail.UpstreamErrors, `"proxy_id":null`)
	require.NotContains(t, detail.UpstreamErrors, `"proxy_mode"`)
	require.NotContains(t, detail.UpstreamErrors, `"proxy_source"`)
	require.NotContains(t, detail.UpstreamErrors, `"proxy_fallback"`)
}

// TestUpstreamRawMatchBody 覆盖 checkSkipMonitoringForUpstreamEvent 迁出去的
// body 优先级派生逻辑：Detail 优先，为空回退 Message，两者皆空返回空字符串
// （MatchRule 仍应被调用以匹配仅按 ErrorCodes 生效的规则）。
func TestUpstreamRawMatchBody(t *testing.T) {
	tests := []struct {
		name    string
		detail  string
		message string
		want    string
	}{
		{"detail non-empty wins", `{"api_key":"secret"}`, "fallback message", `{"api_key":"secret"}`},
		{"detail empty falls back to message", "", "upstream failed", "upstream failed"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, upstreamRawMatchBody(tt.detail, tt.message))
		})
	}
}

// TestAppendOpsUpstreamErrorMatchesRawBodyButStoresSanitizedDetail 是 Finding 4
// 的端到端回归：appendOpsUpstreamError 必须用脱敏前的原始 Detail 做规则匹配
// （否则被打码的关键字永远命不中 skip_monitoring 规则），但落进
// OpsUpstreamErrorsKey 的事件里的 Detail 必须是脱敏后的值（不能因为要匹配
// 规则就把原文写回日志/ops 字段）。
func TestAppendOpsUpstreamErrorMatchesRawBodyButStoresSanitizedDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	const secret = "sk-live-testkeyword123"
	rule := newNonFailoverPassthroughRule(400, secret, 400, "上游请求失败")
	rule.SkipMonitoring = true

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)

	detail := `{"error":{"message":"denied","api_key":"` + secret + `"}}`
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           PlatformAnthropic,
		UpstreamStatusCode: 400,
		Detail:             detail,
	})

	// 落库/日志字段必须是脱敏后的值，不能因为要匹配规则就保留原文。
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.NotContains(t, events[0].Detail, secret)
	require.Contains(t, events[0].Detail, upstreamSensitiveMask)

	// 规则必须按原文命中：secret 出现在 Detail 里，脱敏后会被替换为掩码，
	// 如果匹配用的是脱敏后的文本，这条断言会失败。命中结果记在事件自身
	// （而不是 OpsSkipPassthroughKey）：只有最终对客户端可见的那次失败才决定
	// 是否隐藏，中途已恢复的尝试不能压掉后面的真失败。
	require.True(t, events[0].SkipMonitoring, "skip_monitoring 规则应按原始 Detail 命中")
}
