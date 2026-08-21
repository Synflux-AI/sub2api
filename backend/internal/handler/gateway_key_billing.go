package handler

import (
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// keyBillingInfoSchemaVersion 兼容策略：
//
//	v1 → v2（issue #171）：**纯加性**变更。全部 v1 顶层字段保持原位、原语义
//	（= 默认分组的倍率），旧客户端不需要任何改动即可继续工作。
//	v2 新增 groups[]，逐个列出这把 Key 绑定的每个分组自己的倍率。
//	多分组 Key 的实际计费按**请求命中的分组**走，所以只看顶层字段的旧客户端
//	在多分组场景下看到的是「默认组的倍率」而不是「这次请求的倍率」——
//	想要准确值必须读 groups[]。schema_version 升到 2 就是这个信号。
const keyBillingInfoSchemaVersion = 2

type keyBillingInfoResponse struct {
	Object                  string    `json:"object"`
	SchemaVersion           int       `json:"schema_version"`
	BillingScope            string    `json:"billing_scope"`
	GroupRateMultiplier     float64   `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64  `json:"user_rate_multiplier,omitempty"`
	ResolvedRateMultiplier  float64   `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool      `json:"peak_rate_enabled"`
	PeakStart               *string   `json:"peak_start,omitempty"`
	PeakEnd                 *string   `json:"peak_end,omitempty"`
	PeakRateMultiplier      *float64  `json:"peak_rate_multiplier,omitempty"`
	AppliedPeakMultiplier   *float64  `json:"applied_peak_multiplier,omitempty"`
	EffectiveRateMultiplier float64   `json:"effective_rate_multiplier"`
	Timezone                *string   `json:"timezone,omitempty"`
	ObservedAt              time.Time `json:"observed_at"`

	// Groups 逐个列出全部绑定分组的倍率（issue #171，v2 新增）。
	//
	// 单分组 Key 也会返回一个元素 —— 保持结构统一，客户端不必区分两种形状。
	// 未分组 Key 到不了这里（上面有 403）。
	Groups []keyBillingGroupInfo `json:"groups,omitempty"`
}

// keyBillingGroupInfo 是单个绑定分组的倍率信息，字段与顶层一一对应，
// 便于客户端用同一套解析逻辑处理「默认组」与「某个具体分组」。
//
// **刻意不带 group_id 与 group_name。** 这个端点是客户端可见的，既有测试
// （gateway_key_billing_test.go）与「不得泄露 apiKey.Key」并列地断言了
// 「不得出现分组名」—— 分组名常常编码内部/商务信息。
// 而 platform 本身就是完备的区分键：首版规定每个 Key 每平台至多绑一个分组，
// 所以 platform 唯一确定一个条目；它也不泄露客户端本来不知道的东西
// （客户端自己就在用某个平台的模型）。
type keyBillingGroupInfo struct {
	Platform string `json:"platform"`
	// IsDefault 标出哪一个条目对应顶层字段 —— 客户端据此判断「顶层值来自哪个平台」。
	IsDefault bool `json:"is_default"`

	GroupRateMultiplier     float64  `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64 `json:"user_rate_multiplier,omitempty"`
	ResolvedRateMultiplier  float64  `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool     `json:"peak_rate_enabled"`
	PeakStart               *string  `json:"peak_start,omitempty"`
	PeakEnd                 *string  `json:"peak_end,omitempty"`
	PeakRateMultiplier      *float64 `json:"peak_rate_multiplier,omitempty"`
	AppliedPeakMultiplier   *float64 `json:"applied_peak_multiplier,omitempty"`
	EffectiveRateMultiplier float64  `json:"effective_rate_multiplier"`
}

// KeyBillingInfo returns the token billing multiplier effective for the authenticated API key.
// GET /v1/sub2api/billing
func (h *GatewayHandler) KeyBillingInfo(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if h.cfg != nil && h.cfg.RunMode == config.RunModeSimple {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Billing information is not supported in simple mode")
		return
	}
	if apiKey.GroupID == nil {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	if apiKey.Group == nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Billing information is unavailable")
		return
	}

	resolvedRate, ok := h.resolveKeyBillingRate(c, apiKey)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Billing information is unavailable")
		return
	}

	now := timezone.Now()
	response := buildKeyBillingInfo(apiKey, resolvedRate, now)
	response.Groups = h.buildKeyBillingGroups(c, apiKey, now)

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, response)
}

func (h *GatewayHandler) resolveKeyBillingRate(c *gin.Context, apiKey *service.APIKey) (float64, bool) {
	return h.resolveGroupBillingRate(c, apiKey.UserID, apiKey.Group)
}

// resolveGroupBillingRate 解析某个分组对该用户的最终倍率（含用户专属覆盖）。
//
// 按分组的 platform 分派到 OpenAI 或 Anthropic 侧的 service —— 两侧各自维护
// user:group 倍率缓存，走错一侧会读到未预热的缓存。这个分派规则是从原来只处理
// 默认组的 resolveKeyBillingRate 原样抽出来的，没有改变判定。
func (h *GatewayHandler) resolveGroupBillingRate(c *gin.Context, userID int64, group *service.Group) (float64, bool) {
	if group == nil {
		return 0, false
	}
	groupRate := group.RateMultiplier
	switch group.Platform {
	case service.PlatformOpenAI, service.PlatformGrok:
		if h.openAIGatewayService == nil {
			return 0, false
		}
		return h.openAIGatewayService.ResolveUserGroupRateMultiplier(c.Request.Context(), userID, group.ID, groupRate), true
	default:
		if h.gatewayService == nil {
			return 0, false
		}
		return h.gatewayService.ResolveUserGroupRateMultiplier(c.Request.Context(), userID, group.ID, groupRate), true
	}
}

// buildKeyBillingGroups 逐个分组算出倍率（issue #171）。
//
// 绑定集合为空（老快照或未加载）时退回「只有默认组」一个元素，保证响应结构统一。
func (h *GatewayHandler) buildKeyBillingGroups(c *gin.Context, apiKey *service.APIKey, now time.Time) []keyBillingGroupInfo {
	groups := apiKey.BoundGroups
	if len(groups) == 0 {
		groups = []*service.Group{apiKey.Group}
	}

	defaultGroupID := int64(0)
	if apiKey.GroupID != nil {
		defaultGroupID = *apiKey.GroupID
	}

	out := make([]keyBillingGroupInfo, 0, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		resolved, ok := h.resolveGroupBillingRate(c, apiKey.UserID, g)
		if !ok {
			// 对应平台的 service 没配置：跳过这一组而不是让整个响应 500 ——
			// 其余分组的倍率仍然是有效信息。
			continue
		}
		appliedPeak := g.PeakMultiplierAt(now)
		info := keyBillingGroupInfo{
			Platform:                g.Platform,
			IsDefault:               g.ID == defaultGroupID,
			GroupRateMultiplier:     g.RateMultiplier,
			ResolvedRateMultiplier:  resolved,
			PeakRateEnabled:         g.PeakRateEnabled,
			EffectiveRateMultiplier: resolved * appliedPeak,
		}
		if resolved != g.RateMultiplier {
			// 与顶层同一约定：只有存在用户专属覆盖时才带这个字段。
			r := resolved
			info.UserRateMultiplier = &r
		}
		if g.PeakRateEnabled {
			start, end, mult, applied := g.PeakStart, g.PeakEnd, g.PeakRateMultiplier, appliedPeak
			info.PeakStart = &start
			info.PeakEnd = &end
			info.PeakRateMultiplier = &mult
			info.AppliedPeakMultiplier = &applied
		}
		out = append(out, info)
	}
	return out
}

func buildKeyBillingInfo(apiKey *service.APIKey, resolvedRate float64, now time.Time) keyBillingInfoResponse {
	groupRate := apiKey.Group.RateMultiplier
	var userRate *float64
	if resolvedRate != groupRate {
		userRate = &resolvedRate
	}
	appliedPeak := apiKey.Group.PeakMultiplierAt(now)

	response := keyBillingInfoResponse{
		Object:                  "sub2api.key_billing",
		SchemaVersion:           keyBillingInfoSchemaVersion,
		BillingScope:            "token",
		GroupRateMultiplier:     groupRate,
		UserRateMultiplier:      userRate,
		ResolvedRateMultiplier:  resolvedRate,
		PeakRateEnabled:         apiKey.Group.PeakRateEnabled,
		EffectiveRateMultiplier: resolvedRate * appliedPeak,
		ObservedAt:              now.UTC(),
	}
	if apiKey.Group.PeakRateEnabled {
		response.PeakStart = &apiKey.Group.PeakStart
		response.PeakEnd = &apiKey.Group.PeakEnd
		response.PeakRateMultiplier = &apiKey.Group.PeakRateMultiplier
		response.AppliedPeakMultiplier = &appliedPeak
		tz := timezone.Location().String()
		response.Timezone = &tz
	}
	return response
}
