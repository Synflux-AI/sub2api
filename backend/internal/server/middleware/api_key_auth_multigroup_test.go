//go:build unit

package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// issue #171 的认证状态矩阵。
//
// 这些用例回答的是 issue #144 失败后必须证明的问题：多分组 Key 在认证阶段会不会
// 被**默认组**的状态提前拒绝？以及选组之后，分组门/订阅/余额这些原位不动的检查
// 是否真的作用在生效分组上。
//
// 复用 api_key_auth_test.go 的 stubApiKeyRepo。

type multiGroupFixture struct {
	anthropic *service.Group // 默认组
	openai    *service.Group
	user      *service.User
	apiKey    *service.APIKey
}

func newMultiGroupFixture(t *testing.T) *multiGroupFixture {
	t.Helper()
	anthropic := &service.Group{
		ID: 10, Name: "claude-ccmax", Platform: service.PlatformAnthropic,
		Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard,
		RateMultiplier: 1.0, Hydrated: true,
	}
	openai := &service.Group{
		ID: 20, Name: "codex", Platform: service.PlatformOpenAI,
		Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard,
		RateMultiplier: 1.2, Hydrated: true,
	}
	user := &service.User{ID: 7, Status: service.StatusActive, Role: service.RoleUser, Balance: 100}
	apiKey := &service.APIKey{
		ID: 1, UserID: user.ID, Key: "mg-key", Status: service.StatusActive,
		GroupID: &anthropic.ID, Group: anthropic,
		BoundGroups: []*service.Group{anthropic, openai},
		User:        user,
	}
	return &multiGroupFixture{anthropic: anthropic, openai: openai, user: user, apiKey: apiKey}
}

// effectiveGroupProbeRouter 在 handler 里把生效分组回显出来，这样测试可以断言
// 「这次请求最终按哪个分组走」—— 也就是计费、调度、usage log 会用哪个分组。
func effectiveGroupProbeRouter(t *testing.T, fx *multiGroupFixture, cfg *config.Config) *gin.Engine {
	t.Helper()
	repo := &stubApiKeyRepo{
		getByKey: func(_ context.Context, key string) (*service.APIKey, error) {
			if key != fx.apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *fx.apiKey
			return &clone, nil
		},
	}
	apiKeyService := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)

	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg)))
	probe := func(c *gin.Context) {
		body := gin.H{}
		// ContextKeyAPIKey 里的对象决定了下游 700+ 处 .GroupID / .Group 裸读看到什么。
		if k, ok := GetAPIKeyFromContext(c); ok && k.Group != nil {
			body["api_key_group_id"] = k.Group.ID
			body["api_key_group_platform"] = k.Group.Platform
			body["api_key_group_multiplier"] = k.Group.RateMultiplier
		}
		// ctxkey.Group 是调度与计费实际读的那个。
		if g, ok := c.Request.Context().Value(ctxkey.Group).(*service.Group); ok && g != nil {
			body["ctx_group_id"] = g.ID
		}
		c.JSON(http.StatusOK, body)
	}
	for _, p := range []string{
		"/v1/messages", "/v1/chat/completions", "/v1/live", "/v1/responses",
		"/v1/embeddings", "/v1/videos", "/v1/models", "/v1/usage", "/v1/sub2api/billing",
	} {
		router.POST(p, probe)
		router.GET(p, probe)
	}
	router.GET("/v1/live/:call_id", probe)
	return router
}

func requireJSON(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), out), "响应体不是合法 JSON：%s", w.Body.String())
}

func doProbe(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-api-key", "mg-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// 核心场景：按端点/模型命中不同分组，且倍率随之切换
// ---------------------------------------------------------------------------

func TestAuthMultiGroup_EffectiveGroupFollowsRequest(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeStandard}

	cases := []struct {
		name         string
		method, path string
		body         string
		wantGroupID  float64
		wantMult     float64
	}{
		{"Anthropic 模型命中 Anthropic 分组", http.MethodPost, "/v1/messages", `{"model":"claude-sonnet-4"}`, 10, 1.0},
		{"OpenAI 模型命中 OpenAI 分组", http.MethodPost, "/v1/chat/completions", `{"model":"gpt-5.6"}`, 20, 1.2},
		{"Live 创建（默认组非 OpenAI）仍命中 OpenAI 分组", http.MethodPost, "/v1/live", `{}`, 20, 1.2},
		{"Live sideband 无 body 也命中 OpenAI 分组", http.MethodGet, "/v1/live/abc123", "", 20, 1.2},
		{"Responses WebSocket 无 body 命中 OpenAI 分组", http.MethodGet, "/v1/responses", "", 20, 1.2},
		{"embeddings 按路由锁定 OpenAI", http.MethodPost, "/v1/embeddings", `{"model":"text-embedding-3-small"}`, 20, 1.2},
		{"未知模型回退默认组且不报错", http.MethodPost, "/v1/messages", `{"model":"totally-unknown"}`, 10, 1.0},
		{"无 body 的无平台端点回退默认组", http.MethodGet, "/v1/models", "", 10, 1.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMultiGroupFixture(t)
			router := effectiveGroupProbeRouter(t, fx, cfg)
			w := doProbe(t, router, tc.method, tc.path, tc.body)

			require.Equal(t, http.StatusOK, w.Code, "选组不得让请求被拒：%s", w.Body.String())
			var got map[string]any
			requireJSON(t, w, &got)
			require.EqualValues(t, tc.wantGroupID, got["api_key_group_id"],
				"ContextKeyAPIKey 里的分组决定下游全部 .GroupID 裸读")
			require.EqualValues(t, tc.wantGroupID, got["ctx_group_id"],
				"ctxkey.Group 必须与之一致，否则计费会 fallback 到调度分组（静默错价）")
			require.EqualValues(t, tc.wantMult, got["api_key_group_multiplier"],
				"倍率必须来自实际命中的分组，这是 issue #171 的核心诉求")
		})
	}
}

// Grok 端点在没绑 Grok 分组时必须回退默认组，而不是新造错误。
// 现状（单分组且平台不匹配）是由 handler 返回 404「not supported」，改造后必须一致。
func TestAuthMultiGroup_LockedPlatformWithoutBoundGroupFallsBackNotRejects(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeStandard}
	fx := newMultiGroupFixture(t) // 只绑了 anthropic + openai，没有 grok
	router := effectiveGroupProbeRouter(t, fx, cfg)

	w := doProbe(t, router, http.MethodPost, "/v1/videos", `{"model":"grok-video-1"}`)
	require.Equal(t, http.StatusOK, w.Code, "选组阶段不得为「锁定平台无绑定组」新造错误")
	var got map[string]any
	requireJSON(t, w, &got)
	require.EqualValues(t, 10, got["api_key_group_id"], "回退默认组，交给 handler 既有的 404 兜底")
}

// ---------------------------------------------------------------------------
// #144 的直接死因：默认组不可用时，多分组 Key 会不会被提前拒绝
// ---------------------------------------------------------------------------

func TestAuthMultiGroup_DisabledDefaultGroupDoesNotBlockOtherGroup(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeStandard}

	t.Run("默认组已停用，但请求命中的 OpenAI 分组可用 → 放行", func(t *testing.T) {
		fx := newMultiGroupFixture(t)
		fx.anthropic.Status = service.StatusDisabled
		router := effectiveGroupProbeRouter(t, fx, cfg)

		w := doProbe(t, router, http.MethodPost, "/v1/live", `{}`)
		require.Equal(t, http.StatusOK, w.Code,
			"默认组停用不得拦住命中另一个可用分组的请求 —— 这正是 issue #144 的死因")
		var got map[string]any
		requireJSON(t, w, &got)
		require.EqualValues(t, 20, got["api_key_group_id"])
	})

	t.Run("默认组已停用且请求确实命中默认组 → 仍按原错误码拒绝", func(t *testing.T) {
		fx := newMultiGroupFixture(t)
		fx.anthropic.Status = service.StatusDisabled
		router := effectiveGroupProbeRouter(t, fx, cfg)

		w := doProbe(t, router, http.MethodPost, "/v1/messages", `{"model":"claude-sonnet-4"}`)
		require.Equal(t, http.StatusForbidden, w.Code,
			"命中的分组不可用时必须照旧拒绝，不能因为「还有别的组」就放行")
		requireAPIKeyAuthError(t, w, "GROUP_DISABLED", "API Key 所属分组已停用")
	})

	t.Run("命中的 OpenAI 分组被停用 → 按该分组的状态拒绝", func(t *testing.T) {
		fx := newMultiGroupFixture(t)
		fx.openai.Status = service.StatusDisabled
		router := effectiveGroupProbeRouter(t, fx, cfg)

		w := doProbe(t, router, http.MethodPost, "/v1/live", `{}`)
		require.Equal(t, http.StatusForbidden, w.Code)
		requireAPIKeyAuthError(t, w, "GROUP_DISABLED", "API Key 所属分组已停用")
	})
}

// 专属分组权限也必须按生效分组判定：默认组被撤权，不应挡住命中的另一个组。
func TestAuthMultiGroup_ExclusiveGroupPermissionUsesEffectiveGroup(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeStandard}

	t.Run("默认组是专属且用户已被撤权，但请求命中 OpenAI 分组 → 放行", func(t *testing.T) {
		fx := newMultiGroupFixture(t)
		fx.anthropic.IsExclusive = true
		fx.user.AllowedGroups = []int64{} // 撤权
		router := effectiveGroupProbeRouter(t, fx, cfg)

		w := doProbe(t, router, http.MethodPost, "/v1/live", `{}`)
		require.Equal(t, http.StatusOK, w.Code)
		var got map[string]any
		requireJSON(t, w, &got)
		require.EqualValues(t, 20, got["api_key_group_id"])
	})

	t.Run("命中的分组是专属且已被撤权 → 拒绝", func(t *testing.T) {
		fx := newMultiGroupFixture(t)
		fx.openai.IsExclusive = true
		fx.user.AllowedGroups = []int64{fx.anthropic.ID}
		router := effectiveGroupProbeRouter(t, fx, cfg)

		w := doProbe(t, router, http.MethodPost, "/v1/live", `{}`)
		require.Equal(t, http.StatusForbidden, w.Code)
		requireAPIKeyAuthError(t, w, "GROUP_NOT_ALLOWED", "API Key 所属专属分组不再允许当前用户使用")
	})
}

// ---------------------------------------------------------------------------
// 单分组 / 未分组 Key 的零影响
// ---------------------------------------------------------------------------

// 单分组 Key 必须完全不被选组触碰：body 不能被读走，handler 仍能原样读到。
func TestAuthSingleGroup_BodyIsNotConsumedByGroupSelection(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeStandard}
	fx := newMultiGroupFixture(t)
	fx.apiKey.BoundGroups = []*service.Group{fx.anthropic} // 退回单分组

	repo := &stubApiKeyRepo{
		getByKey: func(_ context.Context, key string) (*service.APIKey, error) {
			clone := *fx.apiKey
			return &clone, nil
		},
	}
	apiKeyService := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)

	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg)))
	router.POST("/v1/messages", func(c *gin.Context) {
		raw, err := c.GetRawData()
		require.NoError(t, err)
		c.JSON(http.StatusOK, gin.H{"body": string(raw), "len": c.Request.ContentLength})
	})

	const payload = `{"model":"claude-sonnet-4","stream":true}`
	w := doProbe(t, router, http.MethodPost, "/v1/messages", payload)
	require.Equal(t, http.StatusOK, w.Code)
	var got map[string]any
	requireJSON(t, w, &got)
	require.Equal(t, payload, got["body"], "单分组 Key 的 body 不得被选组读走或改写")
	require.EqualValues(t, len(payload), got["len"], "ContentLength 也必须原样")
}

// 未分组 Key 在选组阶段不得被提前拒绝（C5）：仍走原有放行分支，
// 由认证之后的 RequireGroupAssignment 兜底。
func TestAuthUngroupedKeyIsNotRejectedByGroupSelection(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeStandard}
	fx := newMultiGroupFixture(t)
	fx.apiKey.GroupID = nil
	fx.apiKey.Group = nil
	fx.apiKey.BoundGroups = nil

	router := effectiveGroupProbeRouter(t, fx, cfg)
	w := doProbe(t, router, http.MethodPost, "/v1/messages", `{"model":"claude-sonnet-4"}`)
	require.Equal(t, http.StatusOK, w.Code, "未分组 Key 必须继续走原有放行分支")
	var got map[string]any
	requireJSON(t, w, &got)
	require.NotContains(t, got, "api_key_group_id", "未分组 Key 不应凭空得到一个分组")
}
