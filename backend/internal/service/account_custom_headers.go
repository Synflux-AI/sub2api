package service

import (
	"net/http"
	"strings"
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
//   - 使用 Set 而非 Add：覆盖此前由 gateway 设置的同名 header（管理员显式覆盖）。
//
// 调用约定：必须在 gateway 完成所有内置 header 设置之后、http client 发起请求之前调用一次。
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
		req.Header.Set(key, value)
	}
}
