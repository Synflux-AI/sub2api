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
