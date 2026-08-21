package middleware

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 本文件是 issue #171 的选组决策层：给定「这把 Key 绑了哪些分组」+「这是个什么请求」，
// 选出**唯一**一个 effective group。
//
// 全部是纯函数：不碰 gin.Context、不查 DB/Redis、不读 body。
// 读 body、取 ForcePlatform、写回 context 都由认证中间件负责（见 api_key_auth.go）。
// 这样选组逻辑可以被穷举测试，而不需要搭一整套 HTTP 脚手架。
//
// 为什么选组在认证中间件**内部**而不是独立的 gin 中间件：分组级 enforcement
// （可用性、专属权限、订阅、订阅限额、余额分支）都在认证中间件里，且顺序决定了
// 客户看到的错误码与 ingress/Ops 标记。把它们搬出去会改变这个顺序，违反
// 「单分组 Key 行为逐字不变」这条最高优先级约束。选组只需要在**分组门之前**
// 落定 effective group，不需要自己成为一个中间件。

// GroupSelectionInput 是选组需要的全部信息。
//
// 调用方（认证中间件）负责填充；本包不主动去任何地方取数据。
type GroupSelectionInput struct {
	// BoundGroups 是这把 Key 的全部绑定分组，每平台至多一个（C1），
	// 已按 (Platform, ID) 稳定排序。
	BoundGroups []*service.Group
	// DefaultGroup 是 api_keys.group_id 指向的默认组，可能为 nil（未分组 Key）。
	DefaultGroup *service.Group
	// ForcePlatform 是路由层已经锁定的平台（当前只有 antigravity 路由会设），
	// 空串表示未锁定。这是最强信号。
	ForcePlatform string
	// RoutePath 是 gin 的路由模式（c.FullPath()），不是原始 URL。
	// 用模式而不是原始路径，是为了让 :param 段不影响匹配。
	RoutePath string
	// CodexClientVersion 表示这是 `GET /models?client_version=...` 这种
	// Codex CLI 的模型探测请求 —— 同一个路由按 query 分流到 OpenAI handler。
	CodexClientVersion bool
	// RequestModel 是从请求体里解析出的模型名；空串表示无 body 或解析不出。
	// **空串不是错误**：107 条网关路由里 54 条完全无 body。
	RequestModel string
}

// SelectEffectiveGroup 按优先级选出本次请求的生效分组。
//
// 优先级（spec §4）：
//  1. ForcePlatform 已锁定的平台；
//  2. 路由明确限定的平台（端点平台锁定表，见 EndpointPlatformLock）；
//  3. 请求模型对应的平台（DetectModelPlatform）；
//  4. 默认分组。
//
// 关于 spec 里的优先级 3「资源记录保存的 group_id」：本版**不实现**，因为首版规定
// 每个平台至多绑定一个分组，于是「锁定平台」之后候选组已经唯一 —— 资源反查得到的
// group 只可能是同一个。唯一真正存了 group_id 的资源是 Live call（Redis
// LiveCallRecord.GroupID）；若那个组已被解绑，反查也选不出它，结果与平台锁定一致。
// Grok 视频的 group 是 sticky key 的**输入**而非输出，本就无法反查。
// 因此在认证热路径上加一次 Redis 查询是纯成本。
// **若将来放开「同平台多组」，必须在这里补资源反查** —— 否则 Live sideband 会选错组并 403。
//
// 选不出来时一律回退默认组，**绝不返回错误**（C6）：模型识别失败、锁定平台没有对应
// 绑定组，都走默认组。让 handler 用它既有的 404「not supported for this platform」
// 兜底，与改造前行为一致；在选组阶段新造 400 会改变客户看到的错误。
func SelectEffectiveGroup(in GroupSelectionInput) *service.Group {
	// 快速路径：绑定集合为空或只有一个（含「只有默认组」）时，结果必然是默认组。
	// 不读 body、不查表、不做任何判断 —— 单分组 Key 的行为因此与改造前逐字相同（C4）。
	if len(in.BoundGroups) <= 1 {
		return in.DefaultGroup
	}

	// 1. ForcePlatform：路由层已经把平台钉死了。
	if platform := strings.TrimSpace(in.ForcePlatform); platform != "" {
		return groupForPlatformOrDefault(in, platform)
	}

	// 2. 端点平台锁定表。
	if platform, locked := EndpointPlatformLock(in.RoutePath, in.CodexClientVersion); locked {
		return groupForPlatformOrDefault(in, platform)
	}

	// 3. 按请求模型探测平台。DetectModelPlatform 对歧义模型 fail-closed，
	//    返回 false 时我们落默认组。
	if in.RequestModel != "" {
		if platform, ok := service.DetectModelPlatform(in.RequestModel); ok {
			return groupForPlatformOrDefault(in, platform)
		}
	}

	// 4. 默认分组。
	return in.DefaultGroup
}

// groupForPlatformOrDefault 在绑定集合里找该平台的分组，找不到就回退默认组。
//
// 找不到时**不继续尝试下一个信号**：那会让请求落到一个平台完全不同的分组上，
// 与改造前（单分组，平台不匹配 → handler 404）的行为不一致。回退默认组则复刻了现状。
func groupForPlatformOrDefault(in GroupSelectionInput, platform string) *service.Group {
	for _, g := range in.BoundGroups {
		if g != nil && g.Platform == platform {
			return g
		}
	}
	return in.DefaultGroup
}

// ---------------------------------------------------------------------------
// 端点平台锁定表
// ---------------------------------------------------------------------------

// EndpointPlatformLock 判断一个路由模式是否把平台钉死了。
//
// 表的组织方式：先处理不带 /v1 前缀的独立命名空间（antigravity、v1beta、
// backend-api/codex），再**剥掉 /v1 前缀**用同一套规则匹配剩下的路径。
//
// 剥前缀这一步很关键：仓库里 36 条根路径别名与 /v1 版一一对应
// （`POST /v1/messages` 与 `POST /messages` 是同一个 handler）。
// 如果把两种形式各写一遍，漏配其中一个就是 issue #144 的复现路径 ——
// 默认组不是 OpenAI 时，走根路径的 Codex 请求会落错组。归一化让这类漏配不可能发生。
//
// 返回 locked=false 表示「这个端点不锁定平台」，交给模型探测或默认组。
func EndpointPlatformLock(routePath string, codexClientVersion bool) (string, bool) {
	p := strings.TrimSpace(routePath)
	if p == "" {
		return "", false
	}

	// 独立命名空间：这些前缀本身就宣告了平台。
	switch {
	case hasPathPrefix(p, "/antigravity"):
		return service.PlatformAntigravity, true
	case hasPathPrefix(p, "/v1beta"):
		// Gemini 原生协议。模型名在 URL 里（/v1beta/models/:model），body 不含 model。
		return service.PlatformGemini, true
	case hasPathPrefix(p, "/backend-api/codex"):
		// Codex 直连。含无 body 的 GET /backend-api/codex/:call_id 与 /models。
		return service.PlatformOpenAI, true
	}

	// 归一化：/v1/xxx 与 /xxx 视作同一个端点。
	// 只剥恰好是 /v1 的那一段，不能误伤 /v1beta（上面已提前返回，这里再防一层）。
	rest := stripV1Prefix(p)

	switch {
	// ── OpenAI 专属 ──
	case rest == "/live" || hasPathPrefix(rest, "/live"):
		// POST /v1/live 的 body 只有可选的 session.model，不可依赖；
		// GET /v1/live/:call_id 完全无 body。统一按前缀锁定。
		return service.PlatformOpenAI, true
	case rest == "/alpha/search":
		return service.PlatformOpenAI, true
	case rest == "/responses" || hasPathPrefix(rest, "/responses"):
		// GET /v1/responses 是 WebSocket，HTTP 侧无 body（model 在首帧里，选组阶段拿不到）。
		return service.PlatformOpenAI, true
	case rest == "/embeddings":
		// 路由层本来就硬闸门 == PlatformOpenAI。
		return service.PlatformOpenAI, true
	case rest == "/models" && codexClientVersion:
		// GET /models?client_version=... 由同一路由分流到 Codex models handler。
		return service.PlatformOpenAI, true

	// ── Grok 专属 ──
	case rest == "/videos" || hasPathPrefix(rest, "/videos"):
		// 生成端点 body 有 model 且与 grok 一致；查询端点（:request_id[/content]）无 body。
		// sticky session key 里编码的 groupID 是**输入**，无法按 request_id 反查分组，
		// 所以这里必须靠前缀锁定 —— 落错组会让 sticky key 命名空间不同而 404。
		return service.PlatformGrok, true
	case rest == "/tts" || rest == "/stt":
		return service.PlatformGrok, true
	case rest == "/custom-voices" || hasPathPrefix(rest, "/custom-voices"):
		// body 无 model（选号模型硬编码 grok-4.5）。
		return service.PlatformGrok, true
	case rest == "/realtime":
		// 无 body，model 走 query。
		return service.PlatformGrok, true
	case rest == "/web_search" || rest == "/x_search":
		// body 无 model。
		return service.PlatformGrok, true

	// ── Gemini 专属 ──
	case rest == "/images/batches" || hasPathPrefix(rest, "/images/batches"):
		// 批量生图只支持 Gemini，但这个限制在 service 层
		// （batch_image_public.go 的 ensureGroupAllowsBatchImage），路由层看不出来，
		// 所以必须在表里显式写死。子路由全部无 body（cancel 是 POST 但不读 body）。
		return service.PlatformGemini, true
	}

	// 其余端点不锁定平台：
	//   - /messages、/chat/completions、/images/generations 等按 body model 探测；
	//   - /models（无 client_version）、/usage、/sub2api/billing、/images/tasks/:task_id
	//     没有平台概念，落默认组。
	return "", false
}

// hasPathPrefix 判断 p 是否以 prefix 开头，且 prefix 后面是路径分隔符或结束。
//
// 不用 strings.HasPrefix 是为了避免 "/v1/videos-archive" 被 "/v1/videos" 误匹配。
func hasPathPrefix(p, prefix string) bool {
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	return len(p) == len(prefix) || p[len(prefix)] == '/'
}

// stripV1Prefix 把 /v1/xxx 归一化成 /xxx；不是 /v1 段开头的原样返回。
func stripV1Prefix(p string) string {
	if p == "/v1" {
		return "/"
	}
	if strings.HasPrefix(p, "/v1/") {
		return p[len("/v1"):]
	}
	return p
}

// GroupSelectionNeedsRequestModel 判断这次选组是否需要解析请求体里的模型名。
//
// 独立成一个判定，是为了让认证中间件能在**不需要**时完全不碰 body。
// 这很重要：读 body 会改变 c.Request.Body 的状态，而 composite 目标平台中间件、
// request_body_limit 的 413 处理都对此敏感。能不读就不读。
func GroupSelectionNeedsRequestModel(in GroupSelectionInput) bool {
	if len(in.BoundGroups) <= 1 {
		return false // 快速路径，结果已定
	}
	if strings.TrimSpace(in.ForcePlatform) != "" {
		return false // 平台已被路由钉死
	}
	if _, locked := EndpointPlatformLock(in.RoutePath, in.CodexClientVersion); locked {
		return false // 端点已锁定平台
	}
	return true
}
