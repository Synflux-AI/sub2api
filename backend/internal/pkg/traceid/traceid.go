// Package traceid 定义跨多级代理链路透传的关联 ID（X-Trace-Id）的头名与校验规则。
//
// 这是 header 常量与归一化规则的唯一实现（middleware 与 service 两侧均从本包导入），
// 避免在多个包中各自定义 "X-Trace-Id" 字面量或各自实现归一化逻辑。
package traceid

import (
	"strings"
)

// Header 是链路关联 ID 的 HTTP 头名。
const Header = "X-Trace-Id"

// MaxBytes 与 middleware 的 maxPersistentRequestIDBytes 保持一致。
const MaxBytes = 64

// Normalize 规整外部可控的 trace id：修剪空白、剔除非法 UTF-8，
// 返回值在非空且不超过 MaxBytes 时 ok 为 true。
// 规则与 middleware.normalizeCorrelationID 等价，两处不得分叉。
func Normalize(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	return value, value != "" && len(value) <= MaxBytes
}
