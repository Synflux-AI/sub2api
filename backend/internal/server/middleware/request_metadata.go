package middleware

import (
	"strings"
	"unicode/utf8"
)

const (
	maxPersistentRequestIDBytes = 64
	maxPersistentUserAgentBytes = 512
)

// normalizePersistentText bounds attacker-controlled metadata before it reaches
// logs or database columns while preserving valid UTF-8 content.
func normalizePersistentText(value string, maxBytes int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// normalizeCorrelationID 用于 request_id / client_request_id 的归一化。
// 规则与 internal/pkg/traceid.Normalize 完全等价（trim + ToValidUTF8 + 非空 + <=64 字节）——
// 这是刻意保留的并行实现（避免 middleware 与 traceid 循环依赖）。修改本函数的字节上限
// 或算法时必须同步修改 traceid.Normalize，并确认 normalize_parity_test.go 仍然通过。
func normalizeCorrelationID(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	return value, value != "" && len(value) <= maxPersistentRequestIDBytes
}
