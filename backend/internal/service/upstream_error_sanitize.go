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

	// upstreamSensitiveKVRegex 匹配纯文本里的 key=value / key: value。
	// 第 3、5 组捕获可选的开闭引号：必须把闭引号一起吃进匹配范围，否则替换后
	// 会留下一个多余的引号（`token: "abc"` → `token: "***""`）。
	upstreamSensitiveKVRegex = regexp.MustCompile(`(?i)\b(x-api-key|api[_-]?key|apikey|authorization|access[_-]?token|refresh[_-]?token|client[_-]?secret|secret|password|passwd|token)\b(\s*[:=]\s*)("?)([^"\s,;)}\]]+)("?)`)

	// upstreamBearerRegex 匹配 "Bearer <token>" 形态。
	upstreamBearerRegex = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`)
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
	return strings.Contains(payload, "data:") || strings.Contains(payload, "event:")
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
		if len(groups) < 6 {
			return match
		}
		return groups[1] + groups[2] + groups[3] + upstreamSensitiveMask + groups[5]
	})
}
