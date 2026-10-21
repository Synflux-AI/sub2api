package service

import (
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// OpsRequestAttributesKey 保存「请求本身的属性」，供 ops 错误日志落库时读取。
//
// 口径与 usage_logs 同名列一致：各转发路径在算出推理强度 / service tier / 图片参数的
// 地方登记，登记的就是成功时写进转发结果、最终落到 usage_logs 的同一个值。失败时还没
// 算出来的属性保持空值（落库 NULL），不做推测。值按请求覆盖写，failover 换号重算时
// 以最后一次登记为准；WebSocket 每个 turn 开始时清空。
const OpsRequestAttributesKey = "ops_request_attributes"

// OpsRequestStartedAtKey 记录请求进入 ops 错误日志中间件的时刻，用于 ops_error_logs.duration_ms；
// WebSocket 每个 turn 开始时重置为该 turn 的起点。
const OpsRequestStartedAtKey = "ops_request_started_at"

// OpsRequestAttributes 是 OpsRequestAttributesKey 下的值。空串 / 0 表示未登记，落库为 NULL。
type OpsRequestAttributes struct {
	ReasoningEffort     string
	ServiceTier         string
	ImageSize           string
	ImageInputSize      string
	ImageResponseFormat string
	// ImageCount 是请求的张数（如 n），不是实际出图数。
	ImageCount int
}

func updateOpsRequestAttributes(c *gin.Context, update func(*OpsRequestAttributes)) {
	if c == nil {
		return
	}
	attrs := GetOpsRequestAttributes(c)
	update(&attrs)
	c.Set(OpsRequestAttributesKey, attrs)
}

// SetOpsReasoningEffort 登记生效后的推理强度（usage_logs.reasoning_effort 口径）。nil 清空。
func SetOpsReasoningEffort(c *gin.Context, effort *string) {
	updateOpsRequestAttributes(c, func(a *OpsRequestAttributes) { a.ReasoningEffort = optionalStringValue(effort) })
}

// SetOpsServiceTier 登记发往上游的 service tier（usage_logs.service_tier 的请求侧值）。nil 清空。
func SetOpsServiceTier(c *gin.Context, tier *string) {
	updateOpsRequestAttributes(c, func(a *OpsRequestAttributes) { a.ServiceTier = optionalStringValue(tier) })
}

// setOpsOpenAIRequestAttributes 登记 OpenAI 系转发结果里 ReasoningEffort / ServiceTier 的同源值；
// outboundServiceTier 与结果构造处传给 resolvedOpenAIUpstreamServiceTier 的参数相同。
func setOpsOpenAIRequestAttributes(c *gin.Context, reasoningEffort, outboundServiceTier *string) {
	SetOpsReasoningEffort(c, reasoningEffort)
	SetOpsServiceTier(c, resolvedOpenAIUpstreamServiceTier(c, outboundServiceTier))
}

// responsesRequestReasoningEffort 是兼容转换路径（Chat Completions / Messages → Responses）
// 写入转发结果的推理强度：转换后请求里非空的 reasoning.effort。
func responsesRequestReasoningEffort(req *apicompat.ResponsesRequest) *string {
	if req == nil || req.Reasoning == nil || req.Reasoning.Effort == "" {
		return nil
	}
	effort := req.Reasoning.Effort
	return &effort
}

// SetOpsImageRequest 登记出图请求的请求侧参数：sizeTier / inputSize 与成功时
// usage_logs.image_size / image_input_size 的请求侧来源相同，count 为请求的张数。
func SetOpsImageRequest(c *gin.Context, sizeTier, inputSize string, count int) {
	updateOpsRequestAttributes(c, func(a *OpsRequestAttributes) {
		a.ImageSize = strings.TrimSpace(sizeTier)
		a.ImageInputSize = strings.TrimSpace(inputSize)
		a.ImageCount = max(count, 0)
	})
}

// ClearOpsAttemptRequestAttributes 在换号开始新一次尝试前清掉按尝试计算的属性
// （推理强度 / service tier 依赖账号映射，出图返回格式依赖账号类型），避免上一次尝试的值
// 残留到本次失败的错误日志里。请求侧的图片尺寸与张数不受影响。
func ClearOpsAttemptRequestAttributes(c *gin.Context) {
	if _, ok := c.Get(OpsRequestAttributesKey); !ok {
		return
	}
	updateOpsRequestAttributes(c, func(a *OpsRequestAttributes) {
		a.ReasoningEffort, a.ServiceTier, a.ImageResponseFormat = "", "", ""
	})
}

// SetOpsImageResponseFormat 登记已确定的出图返回格式（usage_logs.image_response_format 口径）。
func SetOpsImageResponseFormat(c *gin.Context, format string) {
	updateOpsRequestAttributes(c, func(a *OpsRequestAttributes) { a.ImageResponseFormat = normalizeOpsImageResponseFormat(format) })
}

// usage_logs.image_response_format 有 CHECK 约束只收这两个值；其它值不记。
func normalizeOpsImageResponseFormat(format string) string {
	switch format = strings.ToLower(strings.TrimSpace(format)); format {
	case openAIImageResponseFormatURL, openAIImageResponseFormatB64JSON:
		return format
	default:
		return ""
	}
}

// boundForStorage 把超出 ops_error_logs 列宽 / INT 范围的属性记为空（落 NULL）。
// 尺寸等字段是客户端原样传入的，失败请求里尤其可能是垃圾值；错误日志按批写入，
// 一行越界会让同批全部写入失败，所以宁可丢这一个值也不截断成一个对不上的值。
func (a OpsRequestAttributes) boundForStorage() OpsRequestAttributes {
	keep := func(value string, maxLen int) string {
		if len(value) > maxLen {
			return ""
		}
		return value
	}
	a.ReasoningEffort = keep(a.ReasoningEffort, 20)
	a.ServiceTier = keep(a.ServiceTier, 16)
	a.ImageSize = keep(a.ImageSize, 10)
	a.ImageInputSize = keep(a.ImageInputSize, 32)
	a.ImageResponseFormat = keep(a.ImageResponseFormat, 16)
	if a.ImageCount < 0 || a.ImageCount > math.MaxInt32 {
		a.ImageCount = 0
	}
	return a
}

// GetOpsRequestAttributes 返回当前登记的请求属性（未登记时为零值）。
func GetOpsRequestAttributes(c *gin.Context) OpsRequestAttributes {
	if c == nil {
		return OpsRequestAttributes{}
	}
	if value, ok := c.Get(OpsRequestAttributesKey); ok {
		if attrs, ok := value.(OpsRequestAttributes); ok {
			return attrs
		}
	}
	return OpsRequestAttributes{}
}

// OpsRequestElapsedMs 返回自 OpsRequestStartedAtKey 起的耗时（毫秒）；未记录起点时返回 nil。
func OpsRequestElapsedMs(c *gin.Context) *int {
	if c == nil {
		return nil
	}
	value, ok := c.Get(OpsRequestStartedAtKey)
	if !ok {
		return nil
	}
	startedAt, ok := value.(time.Time)
	if !ok || startedAt.IsZero() {
		return nil
	}
	ms := int(time.Since(startedAt).Milliseconds())
	return &ms
}
