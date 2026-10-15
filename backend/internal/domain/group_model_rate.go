package domain

// GroupModelRateMultiplier 分组下按模型单独设置的售价倍率。
// 命中的模型用 RateMultiplier 替换分组默认倍率（不相乘）；Models 支持精确名与末尾 `*` 前缀通配，
// 匹配语义与分组 model_pricing 一致。
type GroupModelRateMultiplier struct {
	Models         []string `json:"models"`
	RateMultiplier float64  `json:"rate_multiplier"`
}
