package traceid

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			name:   "valid",
			input:  "trace-abc-123",
			want:   "trace-abc-123",
			wantOK: true,
		},
		{
			name:   "leading and trailing whitespace",
			input:  "  trace-abc-123  ",
			want:   "trace-abc-123",
			wantOK: true,
		},
		{
			name:   "empty string",
			input:  "",
			want:   "",
			wantOK: false,
		},
		{
			name:   "whitespace only",
			input:  "   \t\n  ",
			want:   "",
			wantOK: false,
		},
		{
			name:   "exactly 64 bytes",
			input:  strings.Repeat("a", MaxBytes),
			want:   strings.Repeat("a", MaxBytes),
			wantOK: true,
		},
		{
			name:   "65 bytes exceeds limit",
			input:  strings.Repeat("a", MaxBytes+1),
			want:   strings.Repeat("a", MaxBytes+1),
			wantOK: false,
		},
		{
			name:   "invalid utf-8 bytes are stripped",
			input:  "trace-\xff\xfe-id",
			want:   "trace--id",
			wantOK: true,
		},
		{
			// 纵深防御：CRLF 在下游已被 zap 编码器与 header writer 挡住，
			// 这里额外拒绝，避免落库/落日志的 trace_id 含控制字符。
			name:   "embedded CRLF rejected",
			input:  "trace\r\nfake-line",
			want:   "trace\r\nfake-line",
			wantOK: false,
		},
		{
			name:   "embedded NUL rejected",
			input:  "trace\x00id",
			want:   "trace\x00id",
			wantOK: false,
		},
		{
			name:   "embedded vertical tab rejected",
			input:  "trace\x0bid",
			want:   "trace\x0bid",
			wantOK: false,
		},
		{
			name:   "embedded DEL rejected",
			input:  "trace\x7fid",
			want:   "trace\x7fid",
			wantOK: false,
		},
		{
			name:   "embedded tab rejected",
			input:  "trace\tid",
			want:   "trace\tid",
			wantOK: false,
		},
		{
			// 首尾的 \r\n 属空白，TrimSpace 先剔除，剩余部分合法
			name:   "surrounding CRLF trimmed then accepted",
			input:  "\r\ntrace-abc\r\n",
			want:   "trace-abc",
			wantOK: true,
		},
		{
			// DEL 不属 unicode.IsSpace，TrimSpace 不会剔除
			name:   "surrounding DEL rejected",
			input:  "\x7ftrace-abc\x7f",
			want:   "\x7ftrace-abc\x7f",
			wantOK: false,
		},
		{
			// 仅覆盖 ASCII 控制字符：C1 是多字节序列，放行
			name:   "C1 control U+0085 accepted",
			input:  "traceid",
			want:   "traceid",
			wantOK: true,
		},
		{
			name:   "zero width space U+200B accepted",
			input:  "trace​id",
			want:   "trace​id",
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Normalize(tc.input)
			if got != tc.want {
				t.Fatalf("Normalize(%q) value = %q, want %q", tc.input, got, tc.want)
			}
			if ok != tc.wantOK {
				t.Fatalf("Normalize(%q) ok = %v, want %v", tc.input, ok, tc.wantOK)
			}
		})
	}
}

func TestHeaderAndMaxBytesConstants(t *testing.T) {
	if Header != "X-Trace-Id" {
		t.Fatalf("Header = %q, want X-Trace-Id", Header)
	}
	if MaxBytes != 64 {
		t.Fatalf("MaxBytes = %d, want 64", MaxBytes)
	}
}
