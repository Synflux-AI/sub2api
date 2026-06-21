package service

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// routingStrategyCacheTTL 控制已启用策略在内存中的缓存时长。
// 策略数量极少，热路径每请求都要读取，因此短 TTL 缓存 + 写操作即时失效即可。
const routingStrategyCacheTTL = 30 * time.Second

// RoutingMatchContext 承载一次请求用于匹配路由策略的属性。
type RoutingMatchContext struct {
	Platform   string
	GroupID    *int64
	Model      string
	ClientType string // claude_code | codex | other
	UserAgent  string
}

// RoutingDecision 是策略评估结果。RestrictIDs 与 PreferIDs 互斥（first-match-wins）。
type RoutingDecision struct {
	RestrictIDs []int64
	PreferIDs   []int64
	MatchedID   int64  // 命中的策略 ID，0 表示无命中
	MatchedName string // 命中的策略名称（用于排障 / dry-run）
	// AccountPriorities 为命中策略中各账号的优先级（id -> 优先级，数值越小越优先）。
	// 命中智能路由策略时非空（未显式配置则各账号默认为 0，即同一优先级）；旧版分组模型路由为 nil。
	// 选择时作为首要排序键，再按负载率 / LRU 决定（相同优先级的账号会被负载均衡）。
	AccountPriorities map[int64]int
}

// HasMatch 报告是否有策略命中。
func (d RoutingDecision) HasMatch() bool { return d.MatchedID != 0 }

// RoutingStrategyService 管理路由策略的 CRUD、缓存与评估。
type RoutingStrategyService struct {
	repo RoutingStrategyRepository

	mu       sync.RWMutex
	cache    []RoutingStrategy
	loadedAt time.Time

	regexCache sync.Map // pattern(string) -> *regexp.Regexp
}

// NewRoutingStrategyService 创建路由策略服务。
func NewRoutingStrategyService(repo RoutingStrategyRepository) *RoutingStrategyService {
	return &RoutingStrategyService{repo: repo}
}

// ---------- 缓存 ----------

// getEnabled 返回缓存的已启用策略（按 priority 升序）。过期则从仓储重载。
func (s *RoutingStrategyService) getEnabled(ctx context.Context) []RoutingStrategy {
	s.mu.RLock()
	if s.cache != nil && time.Since(s.loadedAt) < routingStrategyCacheTTL {
		cached := s.cache
		s.mu.RUnlock()
		return cached
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	// double-check：可能已被其他 goroutine 重载
	if s.cache != nil && time.Since(s.loadedAt) < routingStrategyCacheTTL {
		return s.cache
	}
	items, err := s.repo.ListEnabled(ctx)
	if err != nil {
		// 重载失败时沿用旧缓存（可能为 nil），避免请求链路因策略表抖动而失败
		return s.cache
	}
	s.cache = items
	s.loadedAt = time.Now()
	return s.cache
}

// invalidate 使缓存失效，下次评估时重载。写操作后调用。
func (s *RoutingStrategyService) invalidate() {
	s.mu.Lock()
	s.cache = nil
	s.loadedAt = time.Time{}
	s.mu.Unlock()
}

// ---------- 评估 ----------

// Evaluate 按 priority 升序评估已启用策略，返回首个命中策略的决策（first-match-wins）。
func (s *RoutingStrategyService) Evaluate(ctx context.Context, mc RoutingMatchContext) RoutingDecision {
	strategies := s.getEnabled(ctx)
	for i := range strategies {
		st := &strategies[i]
		if !strategyAppliesToScope(st, mc) {
			continue
		}
		if !s.matchConditions(st, mc) {
			continue
		}
		ids, prioByID := dedupIDsWithPriorities(st.AccountIDs, st.AccountPriorities)
		if len(ids) == 0 {
			continue
		}
		dec := RoutingDecision{
			MatchedID:         st.ID,
			MatchedName:       st.Name,
			AccountPriorities: prioByID,
		}
		if st.Action == RoutingActionPrefer {
			dec.PreferIDs = ids
		} else {
			dec.RestrictIDs = ids
		}
		return dec
	}
	return RoutingDecision{}
}

// strategyAppliesToScope 判断策略的平台/分组作用域是否覆盖当前请求。
func strategyAppliesToScope(st *RoutingStrategy, mc RoutingMatchContext) bool {
	if st.Platform != "" && st.Platform != mc.Platform {
		return false
	}
	if st.GroupID != nil {
		if mc.GroupID == nil || *st.GroupID != *mc.GroupID {
			return false
		}
	}
	return true
}

// matchConditions 按 match_mode 组合策略内的条件。空条件视为命中（作用域内的兜底策略）。
func (s *RoutingStrategyService) matchConditions(st *RoutingStrategy, mc RoutingMatchContext) bool {
	if len(st.Conditions) == 0 {
		return true
	}
	any := st.MatchMode == RoutingMatchModeAny
	for i := range st.Conditions {
		ok := s.matchOne(&st.Conditions[i], mc)
		if any && ok {
			return true
		}
		if !any && !ok {
			return false
		}
	}
	return !any
}

func (s *RoutingStrategyService) matchOne(c *RoutingCondition, mc RoutingMatchContext) bool {
	switch c.Type {
	case RoutingConditionTypeModel:
		return s.matchModel(c.Op, c.Value, mc.Model)
	case RoutingConditionTypeClient:
		if c.Value == RoutingClientAny || c.Value == "" {
			return true
		}
		return mc.ClientType == c.Value
	case RoutingConditionTypeUserAgent:
		return s.matchUserAgent(c.Op, c.Value, mc.UserAgent)
	default:
		return false
	}
}

func (s *RoutingStrategyService) matchModel(op, pattern, model string) bool {
	if model == "" || pattern == "" {
		return false
	}
	switch op {
	case RoutingConditionOpRegex:
		re := s.compileRegex(pattern)
		return re != nil && re.MatchString(model)
	case RoutingConditionOpExact:
		return pattern == model
	default: // wildcard（默认）
		return matchModelPattern(pattern, model)
	}
}

func (s *RoutingStrategyService) matchUserAgent(op, value, ua string) bool {
	if value == "" || ua == "" {
		return false
	}
	if op == RoutingConditionOpRegex {
		re := s.compileRegex(value)
		return re != nil && re.MatchString(ua)
	}
	// contains（默认），大小写不敏感
	return strings.Contains(strings.ToLower(ua), strings.ToLower(value))
}

// compileRegex 返回编译后的正则（带缓存）。无法编译时返回 nil。
func (s *RoutingStrategyService) compileRegex(pattern string) *regexp.Regexp {
	if v, ok := s.regexCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		s.regexCache.Store(pattern, (*regexp.Regexp)(nil))
		return nil
	}
	s.regexCache.Store(pattern, re)
	return re
}

// DetectRoutingClientType 根据 context 与 User-Agent 推断客户端类型。
func DetectRoutingClientType(ctx context.Context, userAgent string) string {
	if IsClaudeCodeClient(ctx) {
		return RoutingClientClaudeCode
	}
	if strings.Contains(strings.ToLower(userAgent), "codex") {
		return RoutingClientCodex
	}
	return RoutingClientOther
}

// UserAgentFromContext 从 context 读取 User-Agent（由网关 handler 写入）。
func UserAgentFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxkey.UserAgent).(string); ok {
		return v
	}
	return ""
}

// SetUserAgentContext 将 User-Agent 写入 context，供下游策略评估使用。
func SetUserAgentContext(ctx context.Context, ua string) context.Context {
	return context.WithValue(ctx, ctxkey.UserAgent, ua)
}

// dedupIDsWithPriorities 在去重账号 ID（保留首次出现顺序、剔除非正数）的同时，
// 构建 id -> 优先级映射。prios 与 ids 按下标对齐；缺失或越界的优先级默认为 0（同一优先级）。
func dedupIDsWithPriorities(ids []int64, prios []int) ([]int64, map[int64]int) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	prioByID := make(map[int64]int, len(ids))
	for i, v := range ids {
		if v <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
		p := 0
		if i < len(prios) {
			p = prios[i]
		}
		prioByID[v] = p
	}
	return out, prioByID
}

func dedupInt64(in []int64) []int64 {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if v <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
