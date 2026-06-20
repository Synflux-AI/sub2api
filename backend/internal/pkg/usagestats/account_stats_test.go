package usagestats

import (
	"math"
	"testing"
)

func TestCacheHitRate(t *testing.T) {
	const eps = 1e-9

	tests := []struct {
		name          string
		input         int64
		cacheRead     int64
		cacheCreation int64
		want          float64
	}{
		{
			// Anthropic 形态：三段都非零。命中率 = 读 / 全部输入。
			name: "anthropic three-part", input: 100, cacheRead: 300, cacheCreation: 100, want: 300.0 / 500.0,
		},
		{
			// OpenAI/Gemini 形态：cacheCreation 恒为 0，input 已不含缓存。
			name: "openai/gemini creation zero", input: 70, cacheRead: 30, cacheCreation: 0, want: 30.0 / 100.0,
		},
		{
			// 全部命中。
			name: "all cached", input: 0, cacheRead: 500, cacheCreation: 0, want: 1.0,
		},
		{
			// 无缓存命中。
			name: "no cache hit", input: 500, cacheRead: 0, cacheCreation: 0, want: 0.0,
		},
		{
			// 分母为 0：无样本，返回 0 而非 NaN。
			name: "zero denominator", input: 0, cacheRead: 0, cacheCreation: 0, want: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CacheHitRate(tt.input, tt.cacheRead, tt.cacheCreation)
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("CacheHitRate returned non-finite value: %v", got)
			}
			if math.Abs(got-tt.want) > eps {
				t.Fatalf("CacheHitRate(%d,%d,%d) = %v, want %v",
					tt.input, tt.cacheRead, tt.cacheCreation, got, tt.want)
			}
		})
	}
}
