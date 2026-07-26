package middleware

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
)

// TestNormalizeCorrelationIDMatchesTraceidNormalize 是 traceid.Normalize 与
// middleware.normalizeCorrelationID 之间的漂移守卫：两者规则要求完全等价
// （trim + ToValidUTF8 + 非空 + <=64 字节 + 拒绝 ASCII 控制字符），
// 任何一方修改而未同步，本测试应失败。
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
		// 内嵌 ASCII 控制字符：两侧都必须拒绝
		{name: "embedded CRLF", input: "trace\r\nfake-line"},
		{name: "embedded LF only", input: "trace\nid"},
		{name: "embedded CR only", input: "trace\rid"},
		{name: "embedded NUL", input: "trace\x00id"},
		{name: "embedded vertical tab", input: "trace\x0bid"},
		{name: "embedded unit separator", input: "trace\x1fid"},
		{name: "embedded DEL", input: "trace\x7fid"},
		{name: "embedded tab", input: "trace\tid"},
		// 首尾的 \t\r\n 属空白，会先被 TrimSpace 去掉；DEL 不属空白，须被拒绝
		{name: "surrounding CRLF trimmed to valid", input: "\r\ntrace-abc\r\n"},
		{name: "surrounding DEL not whitespace", input: "\x7ftrace-abc\x7f"},
		// C1 / Unicode 控制字符：两侧都放行（规则只覆盖 ASCII 控制字符）
		{name: "C1 control U+0085", input: "trace\u0085id"},
		{name: "zero width space U+200B", input: "trace\u200bid"},
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
