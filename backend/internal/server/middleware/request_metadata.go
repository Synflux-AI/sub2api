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
// 规则与 internal/pkg/traceid.Normalize 完全等价（trim + ToValidUTF8 + 非空 + <=64 字节
// + 拒绝 ASCII 控制字符 C0/DEL）——这是刻意保留的并行实现（避免 middleware 与 traceid
// 循环依赖）。修改本函数的字节上限或算法时必须同步修改 traceid.Normalize，
// 并确认 normalize_parity_test.go 仍然通过。
func normalizeCorrelationID(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	return value, value != "" && len(value) <= maxPersistentRequestIDBytes && !hasASCIIControlChar(value)
}

// hasASCIIControlChar 判断字符串是否含 C0 控制字符或 DEL；
// 与 traceid.hasASCIIControl 等价，不得分叉。
func hasASCIIControlChar(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}
