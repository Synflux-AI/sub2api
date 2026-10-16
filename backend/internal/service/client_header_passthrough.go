package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

// 客户端请求头透传（#300）：
//   - 全局白名单（系统设置 client_header_passthrough_allowlist）决定哪些客户端请求头
//     可以逐请求复制到上游；
//   - 账号黑名单（extra.client_header_passthrough_blocklist）从白名单中剔除该账号
//     不接受或不应收到的头。生效集合 = 全局白名单 − 账号黑名单。
//
// 与账号「自定义请求头 / 请求头覆写」的区别：那两者是账号级固定值，会把所有请求钉在
// 同一个值上；透传复制的是每个请求自己的值，所以会话头（如 X-Session-Id）可以透传。
//
// 当前接入的路径：Gemini 原生（ForwardNative）、Gemini messages compat（Forward）、
// Gemini chat completions compat。其余平台与路径尚未接入，见 applyClientHeaderPassthrough。
const (
	// AccountExtraClientHeaderPassthroughBlocklist 账号级「禁止透传的请求头」（JSON 字符串数组）。
	AccountExtraClientHeaderPassthroughBlocklist = "client_header_passthrough_blocklist"

	maxClientHeaderPassthroughEntries     = 64
	maxClientHeaderPassthroughNameLength  = 200
	maxClientHeaderPassthroughValueLength = 8192
)

// clientHeaderPassthroughBlockedNames 禁止加入全局透传白名单的请求头（小写）。
// 口径与 headerOverrideBlockedNames / protectedCustomHeaderNames 对齐：
//   - 逐跳头与传输头：由 HTTP 栈管理，透传会破坏请求传输；
//   - host/content-length/content-type/content-encoding/expect：由网关按实际发出的
//     body 决定，客户端值与改写后的 body 不一致；
//   - 认证头：上游凭据由账号统一注入，禁止客户端凭据混进上游请求；
//   - accept-encoding：强制压缩会破坏网关对上游流式响应的解析；
//   - sec-websocket-*：握手头由拨号器管理；
//   - x-trace-id：由网关按请求注入（见 trace_header.go）。
//
// 与 headerOverrideBlockedNames 不同，这里**不**拉黑 session_id 等会话头：覆写拉黑它们
// 是因为静态值会造成会话串扰，而透传复制的正是每个请求自己的值。
var clientHeaderPassthroughBlockedNames = map[string]struct{}{
	"host":                     {},
	"content-length":           {},
	"content-type":             {},
	"content-encoding":         {},
	"transfer-encoding":        {},
	"connection":               {},
	"keep-alive":               {},
	"proxy-authenticate":       {},
	"proxy-authorization":      {},
	"proxy-connection":         {},
	"te":                       {},
	"trailer":                  {},
	"trailers":                 {},
	"upgrade":                  {},
	"expect":                   {},
	"authorization":            {},
	"x-api-key":                {},
	"x-goog-api-key":           {},
	"cookie":                   {},
	"accept-encoding":          {},
	"sec-websocket-key":        {},
	"sec-websocket-version":    {},
	"sec-websocket-extensions": {},
	"sec-websocket-protocol":   {},
	"sec-websocket-accept":     {},
	"x-trace-id":               {},
}

// IsClientHeaderPassthroughBlockedName 报告请求头（不区分大小写）是否禁止透传。
func IsClientHeaderPassthroughBlockedName(name string) bool {
	_, blocked := clientHeaderPassthroughBlockedNames[strings.ToLower(strings.TrimSpace(name))]
	return blocked
}

// NormalizeClientHeaderPassthroughAllowlist 校验并规范化全局透传白名单：
// 名称去首尾空白并转小写，丢弃空项，按大小写不敏感去重；名称非法或命中受保护头时报错。
// 返回值恒为非 nil 切片，便于序列化为 JSON 数组。
func NormalizeClientHeaderPassthroughAllowlist(names []string) ([]string, error) {
	return normalizeClientHeaderPassthroughNames(names, true, "INVALID_CLIENT_HEADER_PASSTHROUGH_ALLOWLIST")
}

// normalizeClientHeaderPassthroughBlocklist 规范化账号黑名单。受保护头本来就不会透传，
// 写进黑名单是无害的冗余，因此这里不拒绝，只校验名称格式。
func normalizeClientHeaderPassthroughBlocklist(names []string) ([]string, error) {
	return normalizeClientHeaderPassthroughNames(names, false, "INVALID_CLIENT_HEADER_PASSTHROUGH_BLOCKLIST")
}

func normalizeClientHeaderPassthroughNames(names []string, rejectProtected bool, reason string) ([]string, error) {
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		if len(name) > maxClientHeaderPassthroughNameLength || !httpguts.ValidHeaderFieldName(name) {
			return nil, infraerrors.Newf(http.StatusBadRequest, reason, "invalid header name %q", name)
		}
		if rejectProtected && IsClientHeaderPassthroughBlockedName(name) {
			return nil, infraerrors.Newf(http.StatusBadRequest, reason,
				"header %q is protected and cannot be passed through", name)
		}
		if len(out) == maxClientHeaderPassthroughEntries {
			return nil, infraerrors.Newf(http.StatusBadRequest, reason,
				"at most %d header names are allowed", maxClientHeaderPassthroughEntries)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// parseClientHeaderPassthroughAllowlistSetting 解析已落库的白名单 JSON。
// 读取路径容错：非法 JSON 视为空，非法或受保护的条目逐个跳过（兜底名单扩充前保存的旧值）。
func parseClientHeaderPassthroughAllowlistSetting(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return []string{}
	}
	var names []string
	if err := json.Unmarshal([]byte(value), &names); err != nil {
		return []string{}
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		normalized, err := NormalizeClientHeaderPassthroughAllowlist([]string{name})
		if err != nil || len(normalized) == 0 || slices.Contains(out, normalized[0]) {
			continue
		}
		out = append(out, normalized[0])
		if len(out) == maxClientHeaderPassthroughEntries {
			break
		}
	}
	return out
}

// NormalizeClientHeaderPassthroughBlocklistExtra 校验并原地规范化 extra 中的账号黑名单。
// 供账号创建/更新/批量更新的保存路径调用；未携带该字段时为 no-op，规范化后为空则移除该键。
func NormalizeClientHeaderPassthroughBlocklistExtra(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	raw, ok := extra[AccountExtraClientHeaderPassthroughBlocklist]
	if !ok {
		return nil
	}
	names, err := clientHeaderPassthroughNamesFromRaw(raw)
	if err != nil {
		return infraerrors.New(http.StatusBadRequest, "INVALID_CLIENT_HEADER_PASSTHROUGH_BLOCKLIST",
			"client_header_passthrough_blocklist must be an array of header names")
	}
	normalized, err := normalizeClientHeaderPassthroughBlocklist(names)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		delete(extra, AccountExtraClientHeaderPassthroughBlocklist)
		return nil
	}
	extra[AccountExtraClientHeaderPassthroughBlocklist] = normalized
	return nil
}

func clientHeaderPassthroughNamesFromRaw(raw any) ([]string, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("non-string entry %T", item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unexpected type %T", raw)
	}
}

// ClientHeaderPassthroughBlocklist 返回账号禁止透传的请求头集合（小写）。未配置时返回 nil。
// 读取路径容错：格式不对的条目直接跳过，不影响其余条目。
func (a *Account) ClientHeaderPassthroughBlocklist() map[string]struct{} {
	if a == nil || a.Extra == nil {
		return nil
	}
	var names []string
	switch v := a.Extra[AccountExtraClientHeaderPassthroughBlocklist].(type) {
	case []string:
		names = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				names = append(names, s)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

// applyClientHeaderPassthrough 把生效集合（全局白名单 − 账号黑名单）中客户端带了的请求头
// 原样复制到出站请求。
//
// 调用约定：在各路径构造请求、写入认证头之后，injectTraceHeader / ApplyHeaderOverrides /
// ApplyCustomHeaders 之前调用；每次构造出站请求（含重试、换号）都要重新调用，
// account 传当前发出请求的账号，黑名单随账号重新计算。
//
// 行为：
//   - allowlist 为空时为 no-op，出站请求与未接入时逐字节一致；
//   - 只补出站请求上**还没有**的头（大小写不敏感）：网关已写入的认证头、固定 UA、
//     以及有写死白名单的路径上已复制/改写过的头一律保持原值，全局白名单只做追加，
//     同一个头也不会以两种拼写同时出现；
//   - 值必须是合法 UTF-8、不含控制字符（含 CR/LF/TAB/NUL/DEL）、长度不超过 8192，
//     多值头中任一值不合格则整头丢弃；
//   - 受保护头即使出现在 allowlist（如未经校验的旧数据）里也不会透传。
//
// 该函数不记录任何头的值；调用方也不应把透传头写进日志或运维错误记录。
func applyClientHeaderPassthrough(c *gin.Context, req *http.Request, account *Account, allowlist []string) {
	if len(allowlist) == 0 || req == nil || c == nil || c.Request == nil {
		return
	}
	blocklist := account.ClientHeaderPassthroughBlocklist()
	for _, name := range allowlist {
		if name == "" || IsClientHeaderPassthroughBlockedName(name) {
			continue
		}
		if _, blocked := blocklist[name]; blocked {
			continue
		}
		if headerPresentFold(req.Header, name) {
			continue
		}
		values := clientHeaderValuesFold(c.Request.Header, name)
		if len(values) == 0 || !clientHeaderPassthroughValuesValid(values) {
			continue
		}
		if req.Header == nil {
			req.Header = make(http.Header)
		}
		req.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

func headerPresentFold(h http.Header, name string) bool {
	for key := range h {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// clientHeaderValuesFold 收集入站请求中该头的全部值（大小写不敏感）。net/http 服务端
// 已把 key 规范化，这里兜底测试或中间件直接以非 canonical key 写入的情况。
func clientHeaderValuesFold(h http.Header, name string) []string {
	var values []string
	for key, vs := range h {
		if strings.EqualFold(key, name) {
			values = append(values, vs...)
		}
	}
	return values
}

func clientHeaderPassthroughValuesValid(values []string) bool {
	for _, value := range values {
		if value == "" || len(value) > maxClientHeaderPassthroughValueLength || !utf8.ValidString(value) {
			return false
		}
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
	}
	return true
}

// clientHeaderPassthroughAllowlist 读取全局透传白名单；未注入 settingService（如部分测试）时视为未配置。
func (s *GeminiMessagesCompatService) clientHeaderPassthroughAllowlist(ctx context.Context) []string {
	if s == nil || s.settingService == nil {
		return nil
	}
	return s.settingService.GetClientHeaderPassthroughAllowlist(ctx)
}
