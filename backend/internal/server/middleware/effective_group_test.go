//go:build unit

package middleware

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 一把 Key 绑三个平台的分组，倍率各不相同 —— 正是 issue #171 的目标场景。
// 默认组刻意选 anthropic，这样「选组失效」会表现为落到 anthropic，一眼可辨。
func multiGroupInput() GroupSelectionInput {
	anthropic := &service.Group{ID: 10, Name: "claude-ccmax", Platform: service.PlatformAnthropic, RateMultiplier: 1.0}
	openai := &service.Group{ID: 20, Name: "codex", Platform: service.PlatformOpenAI, RateMultiplier: 1.2}
	gemini := &service.Group{ID: 30, Name: "gemini-pro", Platform: service.PlatformGemini, RateMultiplier: 0.8}
	return GroupSelectionInput{
		BoundGroups:  []*service.Group{anthropic, openai, gemini},
		DefaultGroup: anthropic,
	}
}

func withPath(in GroupSelectionInput, path string) GroupSelectionInput {
	in.RoutePath = path
	return in
}

// ---------------------------------------------------------------------------
// 单分组快速路径：C4 / C3
// ---------------------------------------------------------------------------

func TestSelectEffectiveGroup_SingleGroupFastPath(t *testing.T) {
	only := &service.Group{ID: 10, Platform: service.PlatformAnthropic}

	t.Run("只有一个绑定组时无条件返回默认组", func(t *testing.T) {
		in := GroupSelectionInput{
			BoundGroups:  []*service.Group{only},
			DefaultGroup: only,
			// 刻意给出会指向别的平台的强信号，验证快速路径根本不看它们。
			ForcePlatform: service.PlatformOpenAI,
			RoutePath:     "/v1/videos",
			RequestModel:  "gpt-5.6",
		}
		require.Same(t, only, SelectEffectiveGroup(in),
			"单分组 Key 必须逐字保持改造前行为：不看路由、不看模型、不看 ForcePlatform")
	})

	t.Run("未分组 Key 返回 nil 而不是报错", func(t *testing.T) {
		// C5：未分组 Key 必须继续走 validateAPIKeyGroupAvailable 的放行分支，
		// 由 RequireGroupAssignment 兜底 403。选组阶段不得提前拒绝。
		require.Nil(t, SelectEffectiveGroup(GroupSelectionInput{RoutePath: "/v1/messages"}))
	})

	t.Run("返回默认组原对象而不是拷贝", func(t *testing.T) {
		// setGroupContext 的幂等判断与 ctxkey.Group 的对象同一性依赖这一点。
		require.Same(t, only, SelectEffectiveGroup(GroupSelectionInput{
			BoundGroups:  []*service.Group{only},
			DefaultGroup: only,
		}))
	})
}

// ---------------------------------------------------------------------------
// 优先级 1：ForcePlatform
// ---------------------------------------------------------------------------

func TestSelectEffectiveGroup_ForcePlatformWins(t *testing.T) {
	in := multiGroupInput()
	in.ForcePlatform = service.PlatformGemini
	// 路由锁 openai、模型指向 anthropic —— 都应被 ForcePlatform 压过。
	in.RoutePath = "/v1/live"
	in.RequestModel = "claude-sonnet-4"

	got := SelectEffectiveGroup(in)
	require.NotNil(t, got)
	require.EqualValues(t, 30, got.ID, "ForcePlatform 是最高优先级信号")
}

func TestSelectEffectiveGroup_ForcePlatformWithoutBoundGroupFallsBackToDefault(t *testing.T) {
	in := multiGroupInput()
	in.ForcePlatform = service.PlatformGrok // 没绑 grok 组
	in.RoutePath = "/v1/messages"

	got := SelectEffectiveGroup(in)
	require.NotNil(t, got)
	require.EqualValues(t, 10, got.ID,
		"锁定平台没有对应绑定组时回退默认组，交给 handler 既有的 404 兜底，不得新造错误")
}

// ---------------------------------------------------------------------------
// 优先级 2：端点平台锁定表
// ---------------------------------------------------------------------------

// 这一组是 issue #144 的直接复现防线：默认组是 Anthropic 时，
// 这些端点必须仍然命中 OpenAI / Grok / Gemini 分组。
func TestSelectEffectiveGroup_PlatformLockedEndpoints(t *testing.T) {
	cases := []struct {
		path       string
		wantID     int64
		wantReason string
	}{
		// OpenAI 专属，且**全部无可靠的 body model**
		{"/v1/live", 20, "POST /v1/live 的 session.model 可选，不可依赖"},
		{"/v1/live/:call_id", 20, "Live sideband 无 body —— spec 验收项"},
		{"/backend-api/codex/realtime/calls", 20, "Codex realtime"},
		{"/backend-api/codex/:call_id", 20, "Codex sideband 无 body"},
		{"/backend-api/codex/models", 20, "Codex models 无 body"},
		{"/backend-api/codex/responses", 20, "Codex 直连 responses"},
		{"/v1/alpha/search", 20, "alpha search"},
		{"/v1/responses", 20, "GET 是 WebSocket，HTTP 侧无 body"},
		{"/v1/responses/*subpath", 20, "responses 子路径"},
		{"/v1/embeddings", 20, "路由层已硬闸门 openai"},
		// Grok 专属
		{"/v1/videos", 0, "grok 未绑定 → 默认组"}, // 下面单独测绑了 grok 的情形
		// Gemini 专属
		{"/v1beta/models", 30, "Gemini 原生协议"},
		{"/v1beta/models/:model", 30, "模型名在 URL 里，body 无 model"},
		{"/v1beta/models/:model/*modelAction", 30, "Gemini modelAction"},
		{"/v1/images/batches", 30, "批量生图只支持 Gemini，限制在 service 层，必须在表里写死"},
		{"/v1/images/batches/:id/items", 30, "批量生图子路由全部无 body"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := SelectEffectiveGroup(withPath(multiGroupInput(), tc.path))
			require.NotNil(t, got)
			want := tc.wantID
			if want == 0 {
				want = 10 // 默认组
			}
			require.EqualValues(t, want, got.ID, tc.wantReason)
		})
	}
}

// 根路径别名与 /v1 版必须选出同一个分组。
// 漏配根路径就是 #144 的复现路径：走根路径的 Codex 请求会落到默认组。
func TestSelectEffectiveGroup_RootAliasMatchesV1Variant(t *testing.T) {
	pairs := [][2]string{
		{"/v1/live", "/live"},
		{"/v1/live/:call_id", "/live/:call_id"},
		{"/v1/responses", "/responses"},
		{"/v1/embeddings", "/embeddings"},
		{"/v1/alpha/search", "/alpha/search"},
		{"/v1/videos", "/videos"},
		{"/v1/videos/:request_id", "/videos/:request_id"},
		{"/v1/custom-voices", "/custom-voices"},
		{"/v1/tts", "/tts"},
		{"/v1/web_search", "/web_search"},
		{"/v1/messages", "/messages"},
		{"/v1/chat/completions", "/chat/completions"},
	}
	in := multiGroupInput()
	in.BoundGroups = append(in.BoundGroups, &service.Group{ID: 40, Platform: service.PlatformGrok})

	for _, p := range pairs {
		t.Run(p[0], func(t *testing.T) {
			v1 := SelectEffectiveGroup(withPath(in, p[0]))
			root := SelectEffectiveGroup(withPath(in, p[1]))
			require.Equal(t, v1, root,
				"%s 与 %s 是同一个 handler，选组结果必须一致", p[0], p[1])
		})
	}
}

func TestSelectEffectiveGroup_GrokEndpointsHitGrokGroup(t *testing.T) {
	in := multiGroupInput()
	grok := &service.Group{ID: 40, Platform: service.PlatformGrok}
	in.BoundGroups = append(in.BoundGroups, grok)

	for _, path := range []string{
		"/v1/videos", "/v1/videos/generations", "/v1/videos/:request_id",
		"/v1/videos/:request_id/content", "/v1/videos/generations/:request_id",
		"/v1/tts", "/v1/stt", "/v1/realtime",
		"/v1/custom-voices", "/v1/custom-voices/:voice_id", "/v1/custom-voices/:voice_id/audio",
		"/v1/web_search", "/v1/x_search",
	} {
		t.Run(path, func(t *testing.T) {
			got := SelectEffectiveGroup(withPath(in, path))
			require.NotNil(t, got)
			require.EqualValues(t, 40, got.ID)
		})
	}
}

// 锁定平台优先于 body model —— 即使 body 里的模型指向别的平台。
func TestSelectEffectiveGroup_RouteLockBeatsRequestModel(t *testing.T) {
	in := multiGroupInput()
	in.RoutePath = "/v1/live"
	in.RequestModel = "claude-sonnet-4" // 指向 anthropic

	got := SelectEffectiveGroup(in)
	require.NotNil(t, got)
	require.EqualValues(t, 20, got.ID, "路由锁定的平台优先于 body 里的模型")
}

func TestSelectEffectiveGroup_ModelsEndpointLocksOpenAIOnlyWithClientVersion(t *testing.T) {
	in := multiGroupInput()
	in.RoutePath = "/v1/models"

	plain := SelectEffectiveGroup(in)
	require.NotNil(t, plain)
	require.EqualValues(t, 10, plain.ID, "GET /v1/models 不锁定平台，落默认组（并集由 handler 负责）")

	in.CodexClientVersion = true
	codex := SelectEffectiveGroup(in)
	require.NotNil(t, codex)
	require.EqualValues(t, 20, codex.ID, "带 client_version 时同一路由分流到 Codex handler，须锁 openai")
}

// ---------------------------------------------------------------------------
// 优先级 3：按模型探测
// ---------------------------------------------------------------------------

func TestSelectEffectiveGroup_ByRequestModel(t *testing.T) {
	cases := []struct {
		model  string
		wantID int64
		note   string
	}{
		{"claude-sonnet-4", 10, "Anthropic 模型命中 Anthropic 分组"},
		{"gpt-5.6", 20, "OpenAI 模型命中 OpenAI 分组"},
		{"gemini-3.6-flash", 30, "Gemini 模型命中 Gemini 分组"},
		{"anthropic/claude-opus-5", 10, "带 provider 前缀"},
		{"openai/gpt-5.6", 20, "带 provider 前缀"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			in := withPath(multiGroupInput(), "/v1/messages")
			in.RequestModel = tc.model
			got := SelectEffectiveGroup(in)
			require.NotNil(t, got)
			require.EqualValues(t, tc.wantID, got.ID, tc.note)
		})
	}
}

// C6：模型识别失败绝不返回错误，落默认组。
func TestSelectEffectiveGroup_UnknownModelFallsBackToDefaultNotError(t *testing.T) {
	for _, model := range []string{"", "some-unknown-model", "totally-made-up", "   "} {
		in := withPath(multiGroupInput(), "/v1/messages")
		in.RequestModel = model
		got := SelectEffectiveGroup(in)
		require.NotNil(t, got, "模型 %q 识别失败也必须返回一个分组，不能返回 nil", model)
		require.EqualValues(t, 10, got.ID, "识别失败落默认组（绝不 400）")
	}
}

// 模型识别出的平台没有绑定组时，同样落默认组。
func TestSelectEffectiveGroup_ModelPlatformWithoutBoundGroupFallsBackToDefault(t *testing.T) {
	in := withPath(multiGroupInput(), "/v1/messages")
	in.RequestModel = "grok-4.5" // 没绑 grok 组
	got := SelectEffectiveGroup(in)
	require.NotNil(t, got)
	require.EqualValues(t, 10, got.ID)
}

// ---------------------------------------------------------------------------
// EndpointPlatformLock 的边界
// ---------------------------------------------------------------------------

func TestEndpointPlatformLock_Unlocked(t *testing.T) {
	for _, path := range []string{
		"/v1/messages", "/messages", "/v1/messages/count_tokens",
		"/v1/chat/completions", "/v1/images/generations", "/v1/images/edits",
		"/v1/images/generations/async", "/v1/images/tasks/:task_id",
		"/v1/models", "/models", "/v1/usage", "/v1/sub2api/billing",
		"",
	} {
		t.Run(path, func(t *testing.T) {
			_, locked := EndpointPlatformLock(path, false)
			require.False(t, locked, "%q 不应锁定平台", path)
		})
	}
}

// 前缀匹配必须按路径段边界，不能把 /videos-archive 当成 /videos。
func TestEndpointPlatformLock_PrefixMatchRespectsSegmentBoundary(t *testing.T) {
	_, locked := EndpointPlatformLock("/v1/videos-archive", false)
	require.False(t, locked, "/v1/videos-archive 不是 /v1/videos 的子路径")

	_, locked = EndpointPlatformLock("/v1/live-sessions", false)
	require.False(t, locked)

	// /v1beta 不能被 /v1 的剥前缀逻辑误伤。
	platform, locked := EndpointPlatformLock("/v1beta/models", false)
	require.True(t, locked)
	require.Equal(t, service.PlatformGemini, platform)
}

func TestStripV1Prefix(t *testing.T) {
	require.Equal(t, "/messages", stripV1Prefix("/v1/messages"))
	require.Equal(t, "/", stripV1Prefix("/v1"))
	require.Equal(t, "/v1beta/models", stripV1Prefix("/v1beta/models"))
	require.Equal(t, "/messages", stripV1Prefix("/messages"))
	require.Equal(t, "/v1x/y", stripV1Prefix("/v1x/y"))
}
