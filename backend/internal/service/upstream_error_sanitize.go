package service

import (
	"encoding/json"
	"regexp"
	"strings"
)

// upstreamSensitiveMask 是所有被打码内容的统一替换值。
const upstreamSensitiveMask = "***"

var (
	// sensitiveQueryParamRegex 覆盖 URL query 里的凭证参数。
	// 由 gemini_messages_compat_service.go 迁入，行为保持不变。
	sensitiveQueryParamRegex = regexp.MustCompile(`(?i)([?&](?:key|client_secret|access_token|refresh_token)=)[^&"\s]+`)

	// upstreamSensitiveFieldRegex 匹配 JSON key / header 名。
	// 故意不含 signature：thinking block 签名错误的排查完全依赖看到该文案
	// （isThinkingBlockSignatureError，gateway_upstream_response.go:157），
	// 且它不是凭证。
	upstreamSensitiveFieldRegex = regexp.MustCompile(`(?i)^(x-)?(api[_-]?key|apikey|authorization|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|secret|password|passwd|token|cookie|session[_-]?id)$`)

	// upstreamSensitiveKVRegex 匹配纯文本（含降级后的非法 JSON 片段）里的
	// key=value / key: value / "key":"value"。
	//
	// 组号（quote-aware，key 与分隔符之间允许一个可选的闭引号，用来吃掉
	// JSON key 的收尾引号，如 `"api_key":"..."`）：
	//   1 key 名
	//   2 key 后的可选闭引号（JSON key 收尾引号，如有）
	//   3 分隔符 `:` 或 `=`（含前后空白）
	//   4 value 的可选开引号
	//   5 value 内容
	//   6 value 的可选闭引号
	// 4、6 必须整体吃进匹配范围，否则替换后会留下多余的引号
	// （`token: "abc"` → `token: "***""`）。
	//
	// 故意不含裸 `token`：这条规则的输出可能经 sanitizeUpstreamErrorMessage
	// 直接进入面向客户端的错误文案（如 openai_images_responses.go 的
	// OpenAIImagesUpstreamError.Message），而模型侧把 token 计数写成
	// `token: 4096` / `token=4096` 是常见合法文案，裸 token 会把这些数字
	// 误打码。JSON 字段名清单（upstreamSensitiveFieldRegex）里的 token 是
	// `^...$` 锚定的字段名，语义无歧义，予以保留。
	upstreamSensitiveKVRegex = regexp.MustCompile(`(?i)\b(x-api-key|api[_-]?key|apikey|authorization|access[_-]?token|refresh[_-]?token|client[_-]?secret|secret|password|passwd)\b("?)(\s*[:=]\s*)("?)([^"\s,;)}\]]+)("?)`)

	// upstreamBearerRegex 匹配 "Bearer <token>" 形态。
	upstreamBearerRegex = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`)

	// sseLineIndicatorRegex 判断某一行是否是 SSE 帧行（`data:`/`event:` 打头，
	// 允许前导空白）。必须锚定行首，否则单行 JSON 里如果某个字符串字段的值
	// 恰好包含子串 "data:"（如 "authentication data: invalid"），会被误判为
	// SSE payload，从而绕过 JSON 递归脱敏、退化到弱文本正则。
	sseLineIndicatorRegex = regexp.MustCompile(`(?m)^[ \t]*(?:data|event):`)
)

// sanitizeUpstreamErrorMessage 保留原名与签名，供既有调用点继续使用。
func sanitizeUpstreamErrorMessage(msg string) string {
	if msg == "" {
		return msg
	}
	return sanitizeUpstreamErrorPayload(msg)
}

// sanitizeUpstreamErrorPayload 按输入形态分派脱敏：JSON 递归、SSE 逐行、纯文本正则。
//
// 只用于日志与 ops 字段。规则匹配必须使用未改写的原始输入，否则关键字会被
// 掩码打断。
func sanitizeUpstreamErrorPayload(payload string) string {
	if strings.TrimSpace(payload) == "" {
		return payload
	}
	if isLikelySSEPayload(payload) {
		return sanitizeSSEPayload(payload)
	}
	return sanitizeUpstreamErrorPayloadValue(payload)
}

func isLikelySSEPayload(payload string) bool {
	return sseLineIndicatorRegex.MatchString(payload)
}

// sanitizeUpstreamErrorPayloadValue 处理「JSON 或纯文本」，不再识别 SSE，
// 避免 data 行内容本身含 "data:" 时递归。
func sanitizeUpstreamErrorPayloadValue(payload string) string {
	trimmed := strings.TrimSpace(payload)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var decoded any
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			if out, err := json.Marshal(maskJSONSecrets(decoded)); err == nil {
				return string(out)
			}
		}
	}
	return maskTextSecrets(payload)
}

func sanitizeSSEPayload(payload string) string {
	lines := strings.Split(payload, "\n")
	for i, line := range lines {
		if data, ok := extractAnthropicSSEDataLine(line); ok {
			lines[i] = "data: " + sanitizeUpstreamErrorPayloadValue(data)
			continue
		}
		lines[i] = maskTextSecrets(line)
	}
	return strings.Join(lines, "\n")
}

func maskJSONSecrets(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for k, v := range typed {
			if upstreamSensitiveFieldRegex.MatchString(strings.TrimSpace(k)) {
				typed[k] = upstreamSensitiveMask
				continue
			}
			typed[k] = maskJSONSecrets(v)
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = maskJSONSecrets(typed[i])
		}
		return typed
	case string:
		return maskTextSecrets(typed)
	default:
		return value
	}
}

func maskTextSecrets(text string) string {
	if text == "" {
		return text
	}
	text = sensitiveQueryParamRegex.ReplaceAllString(text, `$1`+upstreamSensitiveMask)
	text = upstreamBearerRegex.ReplaceAllString(text, "Bearer "+upstreamSensitiveMask)
	return upstreamSensitiveKVRegex.ReplaceAllStringFunc(text, func(match string) string {
		groups := upstreamSensitiveKVRegex.FindStringSubmatch(match)
		if len(groups) < 7 {
			return match
		}
		return groups[1] + groups[2] + groups[3] + groups[4] + upstreamSensitiveMask + groups[6]
	})
}
