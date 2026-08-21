package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// boundGroupsModelsUnion 计算多分组 Key 在 GET /v1/models 上应该看到的模型并集。
//
// ok=false 表示「这不是多分组 Key」或「并集为空」，调用方应当走原有的单分组逻辑
// （一行都不要改）。并集为空时也交回原逻辑，是为了不在这里复制一份平台默认兜底。
//
// 返回的 platform 是**默认组**的平台，只用来决定响应的**形状**
// （OpenAI / Grok / 其余三种结构不同，无法混排）。客户端是按它配置的那个平台的
// SDK 来解析响应的，所以形状必须跟默认组走；变宽的只是 data 里的模型 ID 集合。
func (h *GatewayHandler) boundGroupsModelsUnion(c *gin.Context, apiKey *service.APIKey) ([]string, string, bool) {
	if h == nil || apiKey == nil || len(apiKey.BoundGroups) <= 1 {
		return nil, "", false
	}
	defaultPlatform := ""
	if apiKey.Group != nil {
		defaultPlatform = apiKey.Group.Platform
	}

	var union []string
	for _, g := range apiKey.BoundGroups {
		if g == nil {
			continue
		}
		// 关键顺序：**每个分组先各自过滤，再合并**。
		// 合并后再过滤是错的 —— 那会让 A 组的 models_list_config 误伤 B 组的模型。
		union = mergeModelIDs(union, h.groupModelIDs(c.Request.Context(), g))
	}
	if len(union) == 0 {
		return nil, "", false
	}
	return union, defaultPlatform, true
}

// groupModelIDs 返回**单个**分组最终对外可见的模型 ID 列表。
//
// 这是把 Models handler 里「取可用模型 → 按平台补默认 → 应用 models_list_config」
// 这段按分组抽出来的版本，复用同样的 helper（compositeAvailableModels /
// GetAvailableModels / customModelsListSource / filterModelsByCustomList /
// defaultModelIDsForPlatform），不是第二份实现。
func (h *GatewayHandler) groupModelIDs(ctx context.Context, g *service.Group) []string {
	if h == nil || h.gatewayService == nil || g == nil {
		return nil
	}
	groupID := &g.ID
	platform := g.Platform

	var available []string
	if platform == service.PlatformComposite {
		available = h.compositeAvailableModels(ctx, groupID)
	} else {
		available = h.gatewayService.GetAvailableModels(ctx, groupID, platform)
	}

	fallback := defaultModelIDsForPlatform(platform)
	if g.CustomModelsListEnabled() {
		return filterModelsByCustomList(
			customModelsListSource(platform, available, fallback),
			fallback,
			g.ModelsListConfig.Models,
		)
	}
	if len(available) > 0 {
		return available
	}
	return fallback
}

// writeModelsForPlatform 复用既有的 writeCustomModelsList 做形状分派
// （OpenAI → openai 结构，Grok → grok 结构，其余 → claude 结构）。
// 不新造响应形状：自定义清单路径本来就走这个分派。
func (h *GatewayHandler) writeModelsForPlatform(c *gin.Context, platform string, models []string) {
	writeCustomModelsList(c, platform, models)
}
