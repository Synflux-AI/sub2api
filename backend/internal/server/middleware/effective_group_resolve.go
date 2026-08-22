package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/reqmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// resolveEffectiveGroup 是 effective_group.go 那层纯函数的**唯一** IO 外壳：
// 从 gin.Context 取信号、按需读一次 body、调纯函数选组，选中非默认组时返回
// 换掉生效分组的 APIKey 浅拷贝。
//
// 返回值必须被调用方赋回 apiKey 变量 —— 从赋值点往下，原有的分组门、订阅查询、
// 订阅限额、余额分支、setGroupContext 全部不需要改动就作用于生效分组。
//
// 单分组 / 未分组 Key 在这里**原样返回入参**（同一个指针），不读 body、不查表、
// 不设任何新 ctx key，因此行为与改造前逐字相同。
func resolveEffectiveGroup(c *gin.Context, apiKey *service.APIKey) *service.APIKey {
	if apiKey == nil || len(apiKey.BoundGroups) <= 1 {
		return apiKey
	}

	in := GroupSelectionInput{
		BoundGroups:        apiKey.BoundGroups,
		DefaultGroup:       apiKey.Group,
		ForcePlatform:      forcePlatformFromContext(c),
		RoutePath:          c.FullPath(),
		CodexClientVersion: strings.TrimSpace(c.Query("client_version")) != "",
	}
	if GroupSelectionNeedsRequestModel(in) {
		in.RequestModel = requestModelForGroupSelection(c)
	}

	selected := SelectEffectiveGroup(in)
	if selected == nil {
		return apiKey
	}
	if apiKey.Group != nil && apiKey.Group.ID == selected.ID {
		// 选中的就是默认组：返回原对象而不是拷贝。
		// setGroupContext 的幂等判断与 ctxkey.Group 的对象同一性依赖这一点。
		return apiKey
	}
	return service.CloneAPIKeyWithGroup(apiKey, selected)
}

func forcePlatformFromContext(c *gin.Context) string {
	value, ok := c.Get(string(ContextKeyForcePlatform))
	if !ok {
		return ""
	}
	platform, _ := value.(string)
	return platform
}

// requestModelForGroupSelection 读一次 body 取出模型名，并把 body 原样放回。
//
// 三条约束：
//
//  1. **只对可能带 body 的方法读**。GET/DELETE 等即使 Body 非 nil 也直接跳过，
//     省掉一次无谓的读取与回写。
//  2. **读失败一律返回空串，不报错**。选组失败会落默认组（C6）。特别是超大 body 的
//     MaxBytesError：这里不能抢先把它变成 413，那会改变错误的产生位置与格式。
//     http.MaxBytesReader 在错误后继续报错，所以真正负责 413 的下游中间件仍会命中它。
//  3. **必须把 body 放回去**。下游（composite 目标平台中间件、handler）还要读它。
//
// 关于与 composite 目标平台中间件抢读 body：不会发生。composite 分组不能与其它分组
// 混绑（C2），所以 composite Key 的绑定集合长度必然是 1，走的是上面的快速路径，
// 根本不会进到这里。本函数只在真正的多分组 Key 上执行。
func requestModelForGroupSelection(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	switch c.Request.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return ""
	}

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		// 不回写 body：读已经失败了，写回一个残缺的缓冲只会掩盖下游的 413/400。
		return ""
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))

	return reqmodel.FromBody(c.GetHeader("Content-Type"), body)
}
