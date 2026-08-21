package service

import (
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
)

// API Key status constants
const (
	StatusAPIKeyActive         = "active"
	StatusAPIKeyDisabled       = "disabled"
	StatusAPIKeyQuotaExhausted = "quota_exhausted"
	StatusAPIKeyExpired        = "expired"
)

// Rate limit window durations
const (
	RateLimitWindow5h = 5 * time.Hour
	RateLimitWindow1d = 24 * time.Hour
	RateLimitWindow7d = 7 * 24 * time.Hour
)

// IsWindowExpired returns true if the window starting at windowStart has exceeded the given duration.
// A nil windowStart is treated as expired — no initialized window means any accumulated usage is stale.
func IsWindowExpired(windowStart *time.Time, duration time.Duration) bool {
	return windowStart == nil || time.Since(*windowStart) >= duration
}

type APIKey struct {
	ID          int64
	UserID      int64
	Key         string
	Name        string
	GroupID     *int64
	Status      string
	IPWhitelist []string
	IPBlacklist []string
	// 预编译的 IP 规则，用于认证热路径避免重复 ParseIP/ParseCIDR。
	CompiledIPWhitelist *ip.CompiledIPRules `json:"-"`
	CompiledIPBlacklist *ip.CompiledIPRules `json:"-"`
	LastUsedAt          *time.Time
	LastUsedIP          *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	User                *User
	Group               *Group
	// BoundGroups 是该 Key 的完整绑定集合（**含默认组** Group），
	// 每个平台最多一个分组（由 api_key_groups 的 (api_key_id, platform) 唯一索引保证）。
	//
	// 读路径语义：只包含未软删的分组；排序稳定 —— 先按 Platform 字典序，再按 ID 升序。
	// 写路径语义：作为 Create / Update(fields.BoundGroups=true) 的绑定入参，
	// 仓库层按 GroupBindingsFromGroups 派生出 []GroupBinding 后整体替换关联表。
	//
	// GroupID / Group 的「默认组」语义不变：默认组同时是绑定集合的成员。
	BoundGroups        []*Group
	CurrentConcurrency int

	// Quota fields
	Quota     float64    // Quota limit in USD (0 = unlimited)
	QuotaUsed float64    // Used quota amount
	ExpiresAt *time.Time // Expiration time (nil = never expires)

	// Rate limit fields
	RateLimit5h   float64    // Rate limit in USD per 5h (0 = unlimited)
	RateLimit1d   float64    // Rate limit in USD per 1d (0 = unlimited)
	RateLimit7d   float64    // Rate limit in USD per 7d (0 = unlimited)
	Usage5h       float64    // Used amount in current 5h window
	Usage1d       float64    // Used amount in current 1d window
	Usage7d       float64    // Used amount in current 7d window
	Window5hStart *time.Time // Start of current 5h window
	Window1dStart *time.Time // Start of current 1d window
	Window7dStart *time.Time // Start of current 7d window
}

func (k *APIKey) IsActive() bool {
	return k.Status == StatusActive
}

// HasRateLimits returns true if any rate limit window is configured
func (k *APIKey) HasRateLimits() bool {
	return k.RateLimit5h > 0 || k.RateLimit1d > 0 || k.RateLimit7d > 0
}

// IsExpired checks if the API key has expired
func (k *APIKey) IsExpired() bool {
	if k.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*k.ExpiresAt)
}

// IsQuotaExhausted checks if the API key quota is exhausted
func (k *APIKey) IsQuotaExhausted() bool {
	if k.Quota <= 0 {
		return false // unlimited
	}
	return k.QuotaUsed >= k.Quota
}

// GetQuotaRemaining returns remaining quota (-1 for unlimited)
func (k *APIKey) GetQuotaRemaining() float64 {
	if k.Quota <= 0 {
		return -1 // unlimited
	}
	remaining := k.Quota - k.QuotaUsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

// GetDaysUntilExpiry returns days until expiry (-1 for never expires)
func (k *APIKey) GetDaysUntilExpiry() int {
	if k.ExpiresAt == nil {
		return -1 // never expires
	}
	duration := time.Until(*k.ExpiresAt)
	if duration < 0 {
		return 0
	}
	return int(duration.Hours() / 24)
}

// EffectiveUsage5h returns the 5h window usage, or 0 if the window has expired.
func (k *APIKey) EffectiveUsage5h() float64 {
	if IsWindowExpired(k.Window5hStart, RateLimitWindow5h) {
		return 0
	}
	return k.Usage5h
}

// EffectiveUsage1d returns the 1d window usage, or 0 if the window has expired.
func (k *APIKey) EffectiveUsage1d() float64 {
	if IsWindowExpired(k.Window1dStart, RateLimitWindow1d) {
		return 0
	}
	return k.Usage1d
}

// EffectiveUsage7d returns the 7d window usage, or 0 if the window has expired.
func (k *APIKey) EffectiveUsage7d() float64 {
	if IsWindowExpired(k.Window7dStart, RateLimitWindow7d) {
		return 0
	}
	return k.Usage7d
}

// GroupBinding 是「API Key ↔ 分组」的一条绑定关系，对应 api_key_groups 表的一行。
//
// Platform 是绑定时从 groups.platform 取的**快照**，与关联表的 platform 列一一对应。
// DB 上 (api_key_id, platform) 有唯一索引，因此同一个 Key 在同一平台上只能有一条绑定。
// 分组自身 platform 事后被改动时快照不会自动跟随（联动属于服务层职责，见 issue #171 T7）。
type GroupBinding struct {
	GroupID  int64
	Platform string
}

// GroupBindingsFromGroups 把领域模型里的 []*Group 派生成仓库写入用的 []GroupBinding。
//
// 规则：跳过 nil；按 GroupID 去重（保留首次出现的 platform 快照）；
// 输出按 (Platform, GroupID) 稳定排序，与 APIKey.BoundGroups 的排序约定一致，
// 这样「同一份绑定集合」在任何调用方手里都会产生完全相同的写入顺序，便于对账与测试。
//
// 注意：本函数**不做**「同平台重复」校验 —— 那是服务层的业务校验（T3），
// 若真有两个同平台分组混进来，仓库写入会撞 idx_api_key_groups_key_platform 唯一索引。
func GroupBindingsFromGroups(groups []*Group) []GroupBinding {
	if len(groups) == 0 {
		return nil
	}
	out := make([]GroupBinding, 0, len(groups))
	seen := make(map[int64]struct{}, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		if _, dup := seen[g.ID]; dup {
			continue
		}
		seen[g.ID] = struct{}{}
		out = append(out, GroupBinding{GroupID: g.ID, Platform: g.Platform})
	}
	SortGroupBindings(out)
	return out
}

// lessGroupBinding 是 (Platform, GroupID) 升序这条**唯一**的绑定集合排序规则。
// SortGroupBindings 与 ResolveDefaultGroupID（默认组 = 该序下的第一个）共用它，
// 保证「排序约定」与「默认组选取规则」永远同源，不会各自漂移。
func lessGroupBinding(a, b GroupBinding) bool {
	if a.Platform != b.Platform {
		return a.Platform < b.Platform
	}
	return a.GroupID < b.GroupID
}

// SortGroupBindings 就地按 (Platform, GroupID) 升序排序。
func SortGroupBindings(bindings []GroupBinding) {
	sort.SliceStable(bindings, func(i, j int) bool {
		return lessGroupBinding(bindings[i], bindings[j])
	})
}

// SortBoundGroups 就地按 (Platform, ID) 升序排序，是 APIKey.BoundGroups 的稳定排序约定。
//
// 排在最后的 nil 元素是防御性处理；比较式本身复用 lessGroupBinding，
// 不再内联第二份 (Platform, ID) 比较逻辑。
func SortBoundGroups(groups []*Group) {
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		switch {
		case a == nil && b == nil:
			return false
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return lessGroupBinding(
			GroupBinding{GroupID: a.ID, Platform: a.Platform},
			GroupBinding{GroupID: b.ID, Platform: b.Platform},
		)
	})
}

// APIKeyListFilters holds optional filtering parameters for listing API keys.
type APIKeyListFilters struct {
	Search string
	Status string
	// GroupID: nil=不筛选, 0=未绑定任何分组, >0=绑定集合**包含**该分组。
	//
	// >0 的语义从 issue #171 起由「api_keys.group_id 等于该值」放宽为
	// 「默认组等于该值 OR api_key_groups 里存在该分组的绑定行」，
	// 这样按分组筛选能命中非默认绑定的 Key。
	GroupID *int64
}
