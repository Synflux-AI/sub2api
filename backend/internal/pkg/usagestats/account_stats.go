package usagestats

// AccountStats 账号使用统计
//
// cost: 账号口径费用（使用 total_cost * account_rate_multiplier）
// standard_cost: 标准费用（使用 total_cost，不含倍率）
// user_cost: 用户/API Key 口径费用（使用 actual_cost，受分组倍率影响）
//
// cache_hit_rate: 缓存命中率（0~1），见 CacheHitRate。
type AccountStats struct {
	Requests            int64   `json:"requests"`
	Tokens              int64   `json:"tokens"`
	InputTokens         int64   `json:"input_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	Cost                float64 `json:"cost"`
	StandardCost        float64 `json:"standard_cost"`
	UserCost            float64 `json:"user_cost"`
}

// CacheHitRate 计算缓存命中率，返回 0~1。
//
// 口径：缓存读取 token 占「全部输入 token」的比例。三家厂商在写入 usage_logs 时，
// input_tokens 均已归一为「不含缓存」的纯输入（Anthropic 原生即不含；Gemini/OpenAI
// 分别在解析/记录阶段减去 cached），因此全部输入 = input + cacheRead + cacheCreation，
// 该公式对三家一致、可跨渠道对比。分母为 0 时返回 0。
//
// 注意：cacheCreation 仅 Anthropic 通常非零，OpenAI/Gemini 恒为 0。
func CacheHitRate(input, cacheRead, cacheCreation int64) float64 {
	total := input + cacheRead + cacheCreation
	if total <= 0 {
		return 0
	}
	return float64(cacheRead) / float64(total)
}
