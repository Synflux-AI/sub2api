package routes

import (
	"sort"
	"strings"
	"testing"

	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/stretchr/testify/require"
)

// 这是 issue #171 防「新增端点漏分类」的唯一可靠手段。
//
// 它枚举**真实注册**的每一条网关路由（不是正则扫源码，所以路由组前缀是准确的），
// 要求每条路由要么被端点平台锁定表锁定平台，要么出现在下面的显式豁免清单里。
// 新加一个端点而不做分类，这个测试就会失败并打印出它的路径。
//
// 为什么这条守卫值得存在：issue #144 就是栽在「Live/Codex 等无 body 专属端点没被
// 分类」上 —— 默认组不是 OpenAI 时，这些请求会静默落到默认组，产生错价与 403。
// 这类漏配不会有编译错误、也不会有运行时报错，只能靠这种覆盖性测试兜住。

// unlockedGatewayRoutes 是**有意**不锁定平台的路由。
//
// 每一条都必须写清理由。允许两类：
//   - 按 body 里的 model 探测平台（模型型端点）；
//   - 没有平台概念，落默认组或做跨分组并集。
var unlockedGatewayRoutes = map[string]string{
	// 模型型端点：平台由 body 里的 model 决定。识别不出时落默认组（C6，绝不 400）。
	// 注意：根路径**没有** POST /messages —— 只有 /v1/messages 与
	// /antigravity/v1/messages。根路径别名只覆盖 count_tokens。这个不对称是既有事实，
	// 由 TestUnlockedGatewayRoutesHasNoStaleEntries 钉住，别凭直觉往这里加 "/messages"。
	"/v1/messages":                 "按 body model 探测",
	"/v1/messages/count_tokens":    "按 body model 探测",
	"/messages/count_tokens":       "按 body model 探测（根路径别名）",
	"/v1/chat/completions":         "按 body model 探测",
	"/chat/completions":            "按 body model 探测（根路径别名）",
	"/v1/images/generations":       "openai 或 grok，按 body model 探测",
	"/images/generations":          "openai 或 grok，按 body model 探测（根路径别名）",
	"/v1/images/edits":             "openai 或 grok，按 body model 探测",
	"/images/edits":                "openai 或 grok，按 body model 探测（根路径别名）",
	"/v1/images/generations/async": "openai 或 grok，按 body model 探测",
	"/images/generations/async":    "openai 或 grok，按 body model 探测（根路径别名）",
	"/v1/images/edits/async":       "openai 或 grok，按 body model 探测",
	"/images/edits/async":          "openai 或 grok，按 body model 探测（根路径别名）",

	// 无平台概念：落默认组。
	"/v1/models":                "跨分组模型并集，由 handler 负责；带 client_version 时锁 openai",
	"/models":                   "同上（根路径别名）",
	"/v1/usage":                 "Key 级额度，无 body、不选账号、不做平台判定",
	"/v1/sub2api/billing":       "逐分组倍率，由 handler 遍历绑定集合；顶层字段用默认组",
	"/v1/images/tasks/:task_id": "异步生图结果快照，Redis 记录只有 user+apikey，无 group、无账号",
	"/images/tasks/:task_id":    "同上（根路径别名）",
}

func TestEveryGatewayRouteIsClassifiedForGroupSelection(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	seen := map[string]struct{}{}
	var unclassified []string
	for _, r := range router.Routes() {
		path := r.Path
		if _, dup := seen[path]; dup {
			continue // 同一路径的不同 HTTP 方法只需分类一次
		}
		seen[path] = struct{}{}

		// 锁定表对 GET /models?client_version= 有 query 依赖，两种取值都算「已分类」。
		if _, locked := servermiddleware.EndpointPlatformLock(path, false); locked {
			continue
		}
		if _, locked := servermiddleware.EndpointPlatformLock(path, true); locked {
			continue
		}
		if _, exempt := unlockedGatewayRoutes[path]; exempt {
			continue
		}
		unclassified = append(unclassified, path+" ["+r.Method+"]")
	}

	sort.Strings(unclassified)
	require.Empty(t, unclassified, strings.Join([]string{
		"下面这些网关路由既没被端点平台锁定表锁定平台，也没列进 unlockedGatewayRoutes：",
		strings.Join(unclassified, "\n  "),
		"",
		"请二选一：",
		"  1. 该端点只能在某个平台上工作 → 在 middleware.EndpointPlatformLock 里加锁定规则；",
		"  2. 该端点按 body model 探测平台、或本身没有平台概念 → 加进 unlockedGatewayRoutes 并写清理由。",
		"",
		"不要为了让测试变绿而随手加豁免：漏分类的专属端点在默认组不是目标平台时会静默落错组，",
		"产生错价与 403，且没有任何报错 —— issue #144 就是这么失败的。",
	}, "\n"))
}

// 豁免清单不能烂掉：列了却已经不存在的路由要及时删，否则它会掩盖真正的漏配
// （比如端点改名后，旧名字还在豁免里，新名字漏分类却看不出来）。
func TestUnlockedGatewayRoutesHasNoStaleEntries(t *testing.T) {
	router := newGatewayRoutesTestRouter()
	registered := map[string]struct{}{}
	for _, r := range router.Routes() {
		registered[r.Path] = struct{}{}
	}

	var stale []string
	for path := range unlockedGatewayRoutes {
		if _, ok := registered[path]; !ok {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale,
		"unlockedGatewayRoutes 里这些路由已经不再注册，请删掉：%v", stale)
}

// 豁免清单里的每一条都必须真的没被锁定表锁住 —— 否则两处规则矛盾，
// 读代码的人无法判断哪个生效。
func TestUnlockedGatewayRoutesDoNotOverlapPlatformLock(t *testing.T) {
	for path := range unlockedGatewayRoutes {
		platform, locked := servermiddleware.EndpointPlatformLock(path, false)
		require.False(t, locked,
			"%s 同时出现在豁免清单和锁定表（锁到 %s）里，两处规则矛盾", path, platform)
	}
}
