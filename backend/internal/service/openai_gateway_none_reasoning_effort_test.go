//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func noneReasoningEffortTestAccount(accountType string, baseURL string, strip bool) *Account {
	account := &Account{
		ID:          9220,
		Name:        "reasoning-effort-account",
		Platform:    PlatformOpenAI,
		Type:        accountType,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
	}
	if baseURL != "" {
		account.Credentials["base_url"] = baseURL
	}
	if strip {
		account.Extra = map[string]any{"openai_strip_none_reasoning_effort": true}
	}
	return account
}

// 判定矩阵：账号 type × strip 开关 × 输入形态。
// 默认（开关关闭）任何账号都原样透传；开关开启后无条件恢复历史摘除行为。
func TestFilterOpenAIResponsesNoneReasoningEffortForAccount(t *testing.T) {
	const thirdParty = "https://compat.example/v1"

	inputs := []struct {
		name string
		body string
		// 透传（strip=false）时的期望
		keepNested    bool
		keepFlat      bool
		keepReasoning bool
		// 摘除（strip=true）时的期望
		stripNested    bool
		stripFlat      bool
		stripReasoning bool
	}{
		{
			name: "nested none only",
			body: `{"reasoning":{"effort":"none"}}`,

			keepNested: true, keepReasoning: true,
		},
		{
			name: "flat none only",
			body: `{"reasoning_effort":"none"}`,

			keepFlat: true,
		},
		{
			name: "nested and flat none",
			body: `{"reasoning":{"effort":"none"},"reasoning_effort":"NONE"}`,

			keepNested: true, keepFlat: true, keepReasoning: true,
		},
		{
			name: "nested none with sibling member",
			body: `{"reasoning":{"effort":" none ","summary":"auto"}}`,

			keepNested: true, keepReasoning: true,
			stripReasoning: true,
		},
		{
			name: "minimal is never stripped here",
			body: `{"reasoning":{"effort":"minimal"},"reasoning_effort":"minimal"}`,

			keepNested: true, keepFlat: true, keepReasoning: true,
			stripNested: true, stripFlat: true, stripReasoning: true,
		},
		{
			name: "unspecified effort",
			body: `{"model":"gpt-5.6-sol"}`,
		},
		{
			name: "non-none effort",
			body: `{"reasoning":{"effort":"high"},"reasoning_effort":"low"}`,

			keepNested: true, keepFlat: true, keepReasoning: true,
			stripNested: true, stripFlat: true, stripReasoning: true,
		},
	}

	accounts := []struct {
		name        string
		accountType string
		baseURL     string
	}{
		{name: "oauth", accountType: AccountTypeOAuth},
		{name: "setup token", accountType: AccountTypeSetupToken},
		{name: "apikey official", accountType: AccountTypeAPIKey},
		{name: "apikey third party", accountType: AccountTypeAPIKey, baseURL: thirdParty},
	}

	for _, acc := range accounts {
		for _, in := range inputs {
			t.Run(acc.name+"/"+in.name+"/default preserves", func(t *testing.T) {
				account := noneReasoningEffortTestAccount(acc.accountType, acc.baseURL, false)
				got, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, []byte(in.body))
				require.NoError(t, err)
				require.JSONEq(t, in.body, string(got), "默认必须原样透传，不得改写请求体")
				require.Equal(t, in.keepNested, gjson.GetBytes(got, "reasoning.effort").Exists())
				require.Equal(t, in.keepFlat, gjson.GetBytes(got, "reasoning_effort").Exists())
				require.Equal(t, in.keepReasoning, gjson.GetBytes(got, "reasoning").Exists())
			})

			t.Run(acc.name+"/"+in.name+"/strip switch on", func(t *testing.T) {
				account := noneReasoningEffortTestAccount(acc.accountType, acc.baseURL, true)
				got, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, []byte(in.body))
				require.NoError(t, err)
				require.Equal(t, in.stripNested, gjson.GetBytes(got, "reasoning.effort").Exists())
				require.Equal(t, in.stripFlat, gjson.GetBytes(got, "reasoning_effort").Exists())
				require.Equal(t, in.stripReasoning, gjson.GetBytes(got, "reasoning").Exists())
			})
		}
	}
}

// reasoning 只剩 none 时整个对象一起摘除，避免给上游留一个空 reasoning。
func TestFilterOpenAIResponsesNoneReasoningEffortDropsEmptiedReasoningObject(t *testing.T) {
	account := noneReasoningEffortTestAccount(AccountTypeAPIKey, "https://compat.example/v1", true)
	got, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, []byte(`{"model":"m","reasoning":{"effort":"none"}}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"m"}`, string(got))
}

// 开关仅在 OpenAI 平台账号上生效；其他平台保持旧版默认摘除行为。
func TestOpenAIStripNoneReasoningEffortSwitchScope(t *testing.T) {
	extra := map[string]any{"openai_strip_none_reasoning_effort": true}
	openAIAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: extra}
	require.True(t, openAIAccount.IsOpenAIStripNoneReasoningEffortEnabled())
	for _, platform := range []string{PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformAnthropic, PlatformGemini} {
		account := &Account{Platform: platform, Type: AccountTypeAPIKey, Extra: extra}
		require.False(t, account.IsOpenAIStripNoneReasoningEffortEnabled(), platform)
		require.True(t, shouldStripOpenAIResponsesNoneReasoningEffort(account), platform)
		got, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, []byte(`{"reasoning":{"effort":"none"}}`))
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(got, "reasoning").Exists(), platform)
		got, err = normalizeOpenAIResponsesReasoningEffortForAccount(account, []byte(`{"reasoning":{"effort":"minimal"}}`))
		require.NoError(t, err)
		require.Equal(t, "minimal", gjson.GetBytes(got, "reasoning.effort").String(), platform)
	}

	require.False(t, (*Account)(nil).IsOpenAIStripNoneReasoningEffortEnabled())
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}).IsOpenAIStripNoneReasoningEffortEnabled())
	require.False(t, (&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"openai_strip_none_reasoning_effort": "true"},
	}).IsOpenAIStripNoneReasoningEffortEnabled(), "非布尔值按关闭处理")
}

func forwardCapturedReasoningBody(t *testing.T, account *Account, body []byte) []byte {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.NotNil(t, upstream.lastBody)
	return upstream.lastBody
}

// 端到端（到上游请求体为止）：第三方 base_url 的 API Key 账号默认透传 none 与 minimal。
func TestForwardDefaultPassesReasoningEffortThroughToUpstream(t *testing.T) {
	account := noneReasoningEffortTestAccount(AccountTypeAPIKey, "https://compat.example/v1", false)

	sent := forwardCapturedReasoningBody(t, account,
		[]byte(`{"model":"gpt-5.6-sol","input":"hi","stream":false,"reasoning":{"effort":"none"},"reasoning_effort":"none"}`))
	require.Equal(t, "none", gjson.GetBytes(sent, "reasoning.effort").String())
	require.Equal(t, "none", gjson.GetBytes(sent, "reasoning_effort").String())

	sent = forwardCapturedReasoningBody(t, account,
		[]byte(`{"model":"gpt-5.6-sol","input":"hi","stream":false,"reasoning":{"effort":"minimal"}}`))
	require.Equal(t, "minimal", gjson.GetBytes(sent, "reasoning.effort").String(),
		"默认不再把 minimal 改写为 none")
}

// 开启开关后与旧线上行为一致：none 被摘除、minimal 改写为 none。
func TestForwardStripSwitchRestoresLegacyReasoningEffortRewrite(t *testing.T) {
	account := noneReasoningEffortTestAccount(AccountTypeAPIKey, "https://compat.example/v1", true)

	sent := forwardCapturedReasoningBody(t, account,
		[]byte(`{"model":"gpt-5.6-sol","input":"hi","stream":false,"reasoning":{"effort":"none"},"reasoning_effort":"none"}`))
	require.False(t, gjson.GetBytes(sent, "reasoning").Exists())
	require.False(t, gjson.GetBytes(sent, "reasoning_effort").Exists())

	sent = forwardCapturedReasoningBody(t, account,
		[]byte(`{"model":"gpt-5.6-sol","input":"hi","stream":false,"reasoning":{"effort":"minimal"}}`))
	require.Equal(t, "none", gjson.GetBytes(sent, "reasoning.effort").String())
}

func TestForwardNonOpenAILegacyChatShapeKeepsMinimalRewrite(t *testing.T) {
	account := &Account{
		ID:          9221,
		Name:        "kimi-responses-account",
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"base_url":     "https://api.moonshot.cn/v1",
			"api_protocol": APIProtocolResponses,
		},
	}

	sent := forwardCapturedReasoningBody(t, account,
		[]byte(`{"model":"kimi-k2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"minimal","stream":false}`))
	require.Equal(t, "none", gjson.GetBytes(sent, "reasoning.effort").String())
	require.False(t, gjson.GetBytes(sent, "reasoning_effort").Exists())
}

func forwardCapturedChatReasoningBody(t *testing.T, account *Account, body []byte) []byte {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.NotNil(t, upstream.lastBody)
	return upstream.lastBody
}

func TestForwardAsChatCompletionsReasoningEffortFollowsStripSwitch(t *testing.T) {
	for _, mode := range []openai_compat.ResponsesSupportMode{
		openai_compat.ResponsesSupportModeForceChatCompletions,
		openai_compat.ResponsesSupportModeForceResponses,
	} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			account := rawChatCompletionsTestAccount()
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(mode)}

			sent := forwardCapturedChatReasoningBody(t, account,
				[]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none","stream":false}`))
			if mode == openai_compat.ResponsesSupportModeForceChatCompletions {
				require.Equal(t, "none", gjson.GetBytes(sent, "reasoning_effort").String())
			} else {
				require.Equal(t, "none", gjson.GetBytes(sent, "reasoning.effort").String())
			}

			account.Extra["openai_strip_none_reasoning_effort"] = true
			sent = forwardCapturedChatReasoningBody(t, account,
				[]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none","stream":false}`))
			require.False(t, gjson.GetBytes(sent, "reasoning_effort").Exists())
			require.False(t, gjson.GetBytes(sent, "reasoning.effort").Exists())

			sent = forwardCapturedChatReasoningBody(t, account,
				[]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"minimal","stream":false}`))
			if mode == openai_compat.ResponsesSupportModeForceChatCompletions {
				require.Equal(t, "none", gjson.GetBytes(sent, "reasoning_effort").String())
			} else {
				require.Equal(t, "none", gjson.GetBytes(sent, "reasoning.effort").String())
			}
		})
	}
}
