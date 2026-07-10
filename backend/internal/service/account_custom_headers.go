package service

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// protectedCustomHeaderNames 列出禁止由 Account.CustomHeaders 覆盖的 header 名（不区分大小写）。
//
// 包含两类：
//   - 由 net/http 库自动管理或会破坏请求语义的：Host、Content-Length。
//   - RFC 7230 定义的 hop-by-hop header，逐跳传输不应跨代理转发。
//
// 业务相关的 header（Authorization、x-api-key、anthropic-version 等）不在黑名单中：
// 管理员显式开启高级模式即代表知情同意，可以为对接企业代理等场景覆盖。
var protectedCustomHeaderNames = map[string]struct{}{
	"host":                {},
	"content-length":      {},
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"trailers":            {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// sanitizeCustomHeaders 规整传入的自定义 header 映射用于持久化：
//   - nil 输入返回 nil（与"未配置"保持一致）。
//   - 修剪 key 两端空白并丢弃空 key。
//   - 同名 key 取最后一次（map 输入天然如此）。
//
// 注意：此处不应用受保护 header 黑名单——黑名单仅在出站合并时生效，
// 这样未来扩展黑名单不会丢失用户已配置的值。
func sanitizeCustomHeaders(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[key] = v
	}
	return out
}

// IsCustomHeadersEnabled 判断当前账户是否开启了自定义出站 header（高级模式）。
// 仅当显式开关为 true 且至少存在一个非空键值对时返回 true。
func (a *Account) IsCustomHeadersEnabled() bool {
	if a == nil || !a.CustomHeadersEnabled {
		return false
	}
	for _, v := range a.CustomHeaders {
		_ = v
		return true
	}
	return false
}

// ApplyCustomHeaders 将账户配置的自定义 header 合并到出站 request 上。
//
// 行为：
//   - 仅当 CustomHeadersEnabled=true 且 CustomHeaders 非空时生效。
//   - 跳过 protectedCustomHeaderNames 中的受保护 header，避免破坏 HTTP 语义或代理逻辑。
//   - 跳过键名为空的条目；对单个键的多次设置取最后一次。
//   - 值支持模板变量展开（见 expandCustomHeaderValue），用于级联部署下注入
//     每请求动态的关联 id，如 `X-Client-Request-ID: {{client_request_id}}`。
//   - 使用 Set 而非 Add：覆盖此前由 gateway 设置的同名 header（管理员显式覆盖）。
//
// 调用约定：必须在 gateway 完成所有内置 header 设置之后、http client 发起请求之前调用一次。
// 各 builder 均以 http.NewRequestWithContext 构造 req，故此处可直接从 req.Context() 取值。
func (a *Account) ApplyCustomHeaders(req *http.Request) {
	if req == nil || !a.IsCustomHeadersEnabled() {
		return
	}
	for name, value := range a.CustomHeaders {
		key := strings.TrimSpace(name)
		if key == "" {
			continue
		}
		if _, blocked := protectedCustomHeaderNames[strings.ToLower(key)]; blocked {
			continue
		}
		expanded, ok := expandCustomHeaderValue(value, req)
		if !ok {
			// 含已知模板变量但其运行时值为空：跳过该 header，
			// 不外发空值/字面模板（例如 ctx 无 client_request_id 时）。
			continue
		}
		req.Header.Set(key, expanded)
	}
}

// expandCustomHeaderValue 展开自定义 header 值中的 `{{var}}` 模板变量。
//
// 支持的变量：
//   - client_request_id: 取 req.Context() 里的 ClientRequestID（端到端关联键）。
//
// 规则：
//   - 无模板标记时原样返回。
//   - 已知变量在运行时值为空 → 返回 ok=false，调用方跳过整个 header。
//   - 未知变量 → 保留字面量（含 `{{ }}`），便于将来扩展且不静默吞掉配置。
//   - 未闭合的 `{{` → 剩余内容原样保留。
func expandCustomHeaderValue(value string, req *http.Request) (string, bool) {
	if !strings.Contains(value, "{{") {
		return value, true
	}

	var b strings.Builder
	rest := value
	skip := false
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		closeIdx := strings.Index(rest[start:], "}}")
		if closeIdx < 0 {
			// 无闭合标记：剩余原样保留
			b.WriteString(rest[start:])
			break
		}
		end := start + closeIdx
		name := strings.TrimSpace(rest[start+2 : end])
		switch name {
		case "client_request_id":
			v := ""
			if req != nil {
				v, _ = req.Context().Value(ctxkey.ClientRequestID).(string)
			}
			v = strings.TrimSpace(v)
			if v == "" {
				skip = true
			}
			b.WriteString(v)
		default:
			// 未知变量：保留字面量 `{{name}}`
			b.WriteString(rest[start : end+2])
		}
		rest = rest[end+2:]
	}

	if skip {
		return "", false
	}
	return b.String(), true
}
