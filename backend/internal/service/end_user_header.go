package service

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

	"golang.org/x/net/http/httpguts"
)

// 终端用户 ID 头（#342）。
//
// 部分上游渠道支持在请求头中携带终端用户 ID，用于把策略拒绝归因并封禁到具体终端用户，
// 而不是暂停整个 API key。多级 sub2api 转发时（入口实例 → 号池实例 → 渠道），号池实例
// 只看到入口实例的一个管理员 key，因此终端用户 ID 必须由入口实例确定并逐级原样携带：
//
//   - 入口：终端用户 ID = 当前请求的 API key ID（见 ResolveEndUserID）；
//   - 中间实例：调用方 key 属于管理员用户且携带合法 X-Sub2api-End-User 时，原样沿用入站值，
//     不替换成自己的 key ID；非管理员 key 携带的入站值一律忽略，防止伪造；
//   - 出站：仅对开启 end_user_header_enabled 的账号写出 <prefix><ID>，头名由账号配置。
//     未开启的账号不写出任何终端用户头，入站的 X-Sub2api-End-User 也不会被原样发往上游。
const (
	// EndUserForwardHeader 是 sub2api 实例之间传递终端用户 ID 的内部请求头。
	EndUserForwardHeader = "X-Sub2api-End-User"

	// DefaultEndUserHeaderName 是账号未配置头名时使用的出站头名（渠道通用约定）。
	DefaultEndUserHeaderName = "X-End-User"

	AccountExtraEndUserHeaderEnabled = "end_user_header_enabled"
	AccountExtraEndUserHeaderName    = "end_user_header_name"
	AccountExtraEndUserHeaderPrefix  = "end_user_header_prefix"

	maxEndUserIDLength           = 128
	maxEndUserHeaderPrefixLength = 32
	maxEndUserHeaderNameLength   = 200
)

// endUserIDPattern 是终端用户 ID（含前缀拼接后的出站值）的合法格式。
var endUserIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// endUserHeaderPrefixPattern 是账号前缀的合法格式（可为空）。
var endUserHeaderPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{0,32}$`)

// IsValidEndUserID 报告值是否为合法的终端用户 ID。
func IsValidEndUserID(value string) bool {
	return len(value) <= maxEndUserIDLength && endUserIDPattern.MatchString(value)
}

// ResolveEndUserID 计算当前请求的终端用户 ID。
// apiKey 为空时返回空串（不注入）。inbound 是入站 X-Sub2api-End-User 的值。
func ResolveEndUserID(apiKey *APIKey, inbound string) string {
	if apiKey == nil || apiKey.ID <= 0 {
		return ""
	}
	if apiKey.User != nil && apiKey.User.IsAdmin() {
		if v := strings.TrimSpace(inbound); IsValidEndUserID(v) {
			return v
		}
	}
	return strconv.FormatInt(apiKey.ID, 10)
}

// WithEndUserID 把终端用户 ID 写入 context；id 为空时原样返回。
func WithEndUserID(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.EndUserID, id)
}

func endUserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxkey.EndUserID).(string)
	return id
}

// IsEndUserHeaderEnabled 返回账号是否开启终端用户 ID 头。字段缺失或类型不对按关闭处理。
func (a *Account) IsEndUserHeaderEnabled() bool {
	if a == nil || a.Extra == nil {
		return false
	}
	enabled, ok := a.Extra[AccountExtraEndUserHeaderEnabled].(bool)
	return ok && enabled
}

// EndUserHeaderName 返回账号配置的出站头名；未配置或非法时回退到 DefaultEndUserHeaderName。
func (a *Account) EndUserHeaderName() string {
	if a != nil && a.Extra != nil {
		if name, ok := a.Extra[AccountExtraEndUserHeaderName].(string); ok {
			name = strings.TrimSpace(name)
			if name != "" && isValidEndUserHeaderName(name) {
				return name
			}
		}
	}
	return DefaultEndUserHeaderName
}

// EndUserHeaderPrefix 返回账号配置的出站值前缀；未配置或非法时为空。
func (a *Account) EndUserHeaderPrefix() string {
	if a == nil || a.Extra == nil {
		return ""
	}
	prefix, ok := a.Extra[AccountExtraEndUserHeaderPrefix].(string)
	if !ok || !endUserHeaderPrefixPattern.MatchString(prefix) {
		return ""
	}
	return prefix
}

// endUserHeaderValue 返回该账号对当前请求应写出的终端用户头值；不应写出时返回空串。
func (a *Account) endUserHeaderValue(ctx context.Context) string {
	if !a.IsEndUserHeaderEnabled() {
		return ""
	}
	id := endUserIDFromContext(ctx)
	if id == "" {
		return ""
	}
	value := a.EndUserHeaderPrefix() + id
	if !IsValidEndUserID(value) {
		return ""
	}
	return value
}

// isValidEndUserHeaderName 报告头名是否可作为终端用户 ID 出站头名。
// 受保护头（认证头、逐跳头等）不可用；内部转发头 X-Sub2api-End-User 虽在透传禁止名单中，
// 但正是入口实例转发给号池实例时要配置的头名，因此单独放行。
func isValidEndUserHeaderName(name string) bool {
	if len(name) > maxEndUserHeaderNameLength || !httpguts.ValidHeaderFieldName(name) {
		return false
	}
	if strings.EqualFold(name, EndUserForwardHeader) {
		return true
	}
	return !IsClientHeaderPassthroughBlockedName(name)
}

// NormalizeEndUserHeaderExtra 校验并原地规范化 extra 中的终端用户 ID 头配置。
// 供账号创建 / 更新 / 批量更新的保存路径调用；未携带相关字段时为 no-op。
func NormalizeEndUserHeaderExtra(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	if raw, ok := extra[AccountExtraEndUserHeaderEnabled]; ok {
		if _, isBool := raw.(bool); !isBool && raw != nil {
			return infraerrors.New(http.StatusBadRequest, "INVALID_END_USER_HEADER",
				"end_user_header_enabled must be a boolean")
		}
	}
	if raw, ok := extra[AccountExtraEndUserHeaderName]; ok {
		name, isString := raw.(string)
		if !isString && raw != nil {
			return infraerrors.New(http.StatusBadRequest, "INVALID_END_USER_HEADER",
				"end_user_header_name must be a string")
		}
		name = strings.TrimSpace(name)
		if name == "" {
			delete(extra, AccountExtraEndUserHeaderName)
		} else if !isValidEndUserHeaderName(name) {
			return infraerrors.Newf(http.StatusBadRequest, "INVALID_END_USER_HEADER",
				"invalid or protected end user header name %q", name)
		} else {
			extra[AccountExtraEndUserHeaderName] = name
		}
	}
	if raw, ok := extra[AccountExtraEndUserHeaderPrefix]; ok {
		prefix, isString := raw.(string)
		if !isString && raw != nil {
			return infraerrors.New(http.StatusBadRequest, "INVALID_END_USER_HEADER",
				"end_user_header_prefix must be a string")
		}
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			delete(extra, AccountExtraEndUserHeaderPrefix)
		} else if !endUserHeaderPrefixPattern.MatchString(prefix) {
			return infraerrors.Newf(http.StatusBadRequest, "INVALID_END_USER_HEADER",
				"end_user_header_prefix must match [A-Za-z0-9_.:-] and be at most %d characters",
				maxEndUserHeaderPrefixLength)
		} else {
			extra[AccountExtraEndUserHeaderPrefix] = prefix
		}
	}
	return nil
}

// injectEndUserHeader 在出站请求上落实终端用户 ID 头。调用约定与 injectTraceHeader 相同：
// 在客户端请求头白名单拷贝之后、ApplyHeaderOverrides / ApplyCustomHeaders 之前调用，
// 每次构造出站请求（含重试、换号）都按当前账号重新计算；Bedrock 路径须在签名之前调用。
//
// ctx 传承载入站请求的原始 context（由 API Key 认证中间件写入 ctxkey.EndUserID）。
func injectEndUserHeader(ctx context.Context, req *http.Request, account *Account) {
	if req == nil {
		return
	}
	applyEndUserHeader(ctx, req.Header, account)
}

// applyEndUserHeader 是 injectEndUserHeader 的 http.Header 版本，供 WebSocket 握手头构造使用。
//
// 行为：
//   - 无论账号是否开启，都先删除内部转发头 X-Sub2api-End-User 的所有大小写形式，
//     保证入站值不会被原样发往上游；
//   - 账号开启且当前请求有终端用户 ID 时，删除配置头名的所有大小写形式后写入
//     <prefix><ID>，保证最终出站值唯一且由网关决定（覆盖客户端可能自带的同名头）。
//
// 不记录任何头的值。
func applyEndUserHeader(ctx context.Context, h http.Header, account *Account) {
	if h == nil {
		return
	}
	deleteHeaderFold(h, EndUserForwardHeader)
	value := account.endUserHeaderValue(ctx)
	if value == "" {
		return
	}
	name := account.EndUserHeaderName()
	deleteHeaderFold(h, name)
	h.Set(name, value)
}

// deleteHeaderFold 删除头的所有大小写形式（含 addHeaderRaw 写入的非 canonical key）。
func deleteHeaderFold(h http.Header, name string) {
	for key := range h {
		if strings.EqualFold(key, name) {
			delete(h, key)
		}
	}
}
