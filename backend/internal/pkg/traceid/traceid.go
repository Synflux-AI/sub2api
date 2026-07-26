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
// 并拒绝含 ASCII 控制字符（C0 0x00-0x1F 与 DEL 0x7F）的值。
// 返回值在非空、不超过 MaxBytes 且无控制字符时 ok 为 true。
// 规则与 middleware.normalizeCorrelationID 等价，两处不得分叉。
//
// 控制字符是纵深防御，不是修补现存漏洞：CRLF 注入在下游已经被挡住两层
// （zap 编码器把 \r\n 转义成字面量；Go 的 header writer 把 \r\n 换成空格），
// 这里拒绝是为了让入库/落日志的 trace_id 只含可打印字符。
// 只覆盖 ASCII 控制字符：C1（U+0080-U+009F）与 Unicode 格式字符（如 U+200B）
// 在 UTF-8 下是多字节序列，既不会被 header writer 特殊处理，也不构成日志伪造，
// 且 trace id 由上游代理生成、正常取值是 ASCII，不值得为此引入 unicode 表扫描。
func Normalize(value string) (string, bool) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	return value, value != "" && len(value) <= MaxBytes && !hasASCIIControl(value)
}

// hasASCIIControl 判断字符串是否含 C0 控制字符或 DEL。
func hasASCIIControl(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}
