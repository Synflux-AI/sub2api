package middleware

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
)

// TestNormalizeCorrelationIDMatchesTraceidNormalize 是 traceid.Normalize 与
// middleware.normalizeCorrelationID 之间的漂移守卫：两者规则要求完全等价
// （trim + ToValidUTF8 + 非空 + <=64 字节），任何一方修改而未同步，本测试应失败。
func TestNormalizeCorrelationIDMatchesTraceidNormalize(t *testing.T) {
	multiByteBoundary := strings.Repeat("a", 62) + "中" // 62 + 3 = 65 字节，跨越 64 字节边界

	cases := []struct {
		name  string
		input string
	}{
		{name: "valid", input: "trace-abc-123"},
		{name: "whitespace padded", input: "  trace-abc-123  "},
		{name: "empty", input: ""},
		{name: "whitespace only", input: "   \t\n  "},
		{name: "exactly 64 bytes", input: strings.Repeat("a", 64)},
		{name: "65 bytes", input: strings.Repeat("a", 65)},
		{name: "invalid utf-8", input: "trace-\xff\xfe-id"},
		{name: "multi-byte utf-8 straddling 64-byte boundary", input: multiByteBoundary},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantValue, wantOK := normalizeCorrelationID(tc.input)
			gotValue, gotOK := traceid.Normalize(tc.input)
			if gotValue != wantValue || gotOK != wantOK {
				t.Fatalf("traceid.Normalize(%q) = (%q, %v), normalizeCorrelationID(%q) = (%q, %v); must agree",
					tc.input, gotValue, gotOK, tc.input, wantValue, wantOK)
			}
		})
	}
}
