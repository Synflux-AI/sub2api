package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

// CyberSessionBlockStore 是 cyber 会话屏蔽表的存取接口。
// repository 层 gatewayCache 附带实现（类型断言探测接入，不改 GatewayCache
// 共享接口）；测试 stub 不实现时屏蔽能力自动降级关闭。
type CyberSessionBlockStore interface {
	SetCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string, ttl time.Duration) error
	IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error)
	FindCyberSessionBlocked(ctx context.Context, keys []string) (string, error)
	// AddCyberSessionScopePositions 记录作用域内被屏蔽 transcript 前缀的位置，
	// CyberSessionScopePositions 读回（最多 MaxCyberSessionScopePositions 个）。
	AddCyberSessionScopePositions(ctx context.Context, scopeKey string, positions []int, ttl time.Duration) error
	CyberSessionScopePositions(ctx context.Context, scopeKey string) ([]int, error)
}

// MaxCyberSessionScopePositions 限制超长对话额外查询的前缀数。粗作用域可能被
// 多个终端用户共享（如转售渠道共用一个 Key 与出口 IP），每次命中最多贡献两个位置。
const MaxCyberSessionScopePositions = 64

// CyberSessionExplicitBlockKey returns an inexpensive exact key when the
// client supplies a stable session signal.
func CyberSessionExplicitBlockKey(apiKeyID int64, c *gin.Context, body []byte) string {
	return hashCyberSessionBlockKey(apiKeyID, explicitOpenAISessionID(c, body))
}

// CyberSessionTranscriptBlockKeys returns the exact full-request key followed
// by an optional rewrite-tolerant context key. The latter is emitted only after
// model-generated history has been observed.
func CyberSessionTranscriptBlockKeys(apiKeyID int64, body []byte) []string {
	keys, _ := CyberSessionTranscriptBlockKeysWithPositions(apiKeyID, body)
	return keys
}

// CyberSessionTranscriptBlockKeysWithPositions 同 CyberSessionTranscriptBlockKeys，
// 并返回每个 key 的前缀位置，供超长续写会话按位置精确匹配。
func CyberSessionTranscriptBlockKeysWithPositions(apiKeyID int64, body []byte) ([]string, []int) {
	derived := deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body)
	if len(derived.lookupKeys) == 0 {
		return nil, nil
	}
	keys := []string{derived.lookupKeys[len(derived.lookupKeys)-1]}
	positions := []int{derived.itemCount}
	if derived.preLatestUserKey != "" && derived.preLatestUserKey != keys[0] {
		keys = append(keys, derived.preLatestUserKey)
		positions = append(positions, derived.preLatestUserPos)
	}
	return keys, positions
}

func CyberSessionTranscriptLookupKeys(apiKeyID int64, body []byte) []string {
	return deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body).lookupKeys
}

// CyberSessionScopeKey is a coarse, non-blocking fingerprint used only to
// avoid transcript parsing and MGET for sources that never produced a hit.
func CyberSessionScopeKey(apiKeyID int64, clientIP, userAgent string) string {
	if apiKeyID <= 0 {
		return ""
	}
	raw := "cyber-scope:v1|api_key=" + strconv.FormatInt(apiKeyID, 10) +
		"|ip=" + strings.TrimSpace(clientIP) +
		"|ua=" + NormalizeSessionUserAgent(userAgent)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func hashCyberSessionBlockKey(apiKeyID int64, raw string) string {
	if raw == "" {
		return ""
	}
	isolated := isolateOpenAISessionID(apiKeyID, raw)
	sum := sha256.Sum256([]byte(isolated))
	return hex.EncodeToString(sum[:])
}

// cyberSessionBlockStore 探测 cache 是否具备屏蔽存储能力。
// 注意：若未来以装饰器包装 GatewayCache（如日志/指标装饰器），该装饰器必须同时实现
// CyberSessionBlockStore，否则会话屏蔽能力将静默降级关闭
// （编译断言 var _ service.CyberSessionBlockStore = (*gatewayCache)(nil) 只覆盖
// *gatewayCache 本体，无法覆盖其外层包装）。
func (s *OpenAIGatewayService) cyberSessionBlockStore() CyberSessionBlockStore {
	if s == nil || s.cache == nil {
		return nil
	}
	store, ok := s.cache.(CyberSessionBlockStore)
	if !ok {
		return nil
	}
	return store
}

// CyberSessionBlockRuntime 返回 (开关, TTL)。开关默认关。
// 委托给 SettingService.GetCyberSessionBlockRuntime，进程内缓存避免热路径 DB 往返。
func (s *OpenAIGatewayService) CyberSessionBlockRuntime(ctx context.Context) (bool, time.Duration) {
	if s == nil || s.settingService == nil {
		return false, time.Hour
	}
	return s.settingService.GetCyberSessionBlockRuntime(ctx)
}

// MarkCyberSessionBlocked 把会话写入屏蔽表（写入点：cyber 命中后）。
// positions 是 transcript key 的前缀位置，先于作用域激活写入。
// 开关关闭、key 为空或存储不可用时静默跳过。
func (s *OpenAIGatewayService) MarkCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string, positions []int) {
	if s == nil || len(keys) == 0 {
		return
	}
	enabled, ttl := s.CyberSessionBlockRuntime(ctx)
	if !enabled {
		return
	}
	store := s.cyberSessionBlockStore()
	if store == nil {
		return
	}
	if scopeKey != "" && len(positions) > 0 {
		if err := store.AddCyberSessionScopePositions(ctx, scopeKey, positions, ttl); err != nil {
			logger.LegacyPrintf("service.openai_gateway", "cyber session scope positions write failed: err=%v", err)
		}
	}
	if err := store.SetCyberSessionBlocked(ctx, scopeKey, keys, ttl); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session block write failed: err=%v", err)
	}
}

// FindCyberSessionBlockedForRequest applies explicit-first lookup followed by
// scope-gated transcript matching. All failures remain fail-open.
func (s *OpenAIGatewayService) FindCyberSessionBlockedForRequest(ctx context.Context, apiKeyID int64, c *gin.Context, body []byte, clientIP, userAgent string) string {
	enabled, _ := s.CyberSessionBlockRuntime(ctx)
	if !enabled {
		return ""
	}
	store := s.cyberSessionBlockStore()
	if store == nil {
		return ""
	}
	if explicitKey := CyberSessionExplicitBlockKey(apiKeyID, c, body); explicitKey != "" {
		key, err := store.FindCyberSessionBlocked(ctx, []string{explicitKey})
		if err != nil {
			logger.LegacyPrintf("service.openai_gateway", "cyber explicit session read failed: err=%v", err)
			return ""
		}
		if key != "" {
			return key
		}
	}
	scopeKey := CyberSessionScopeKey(apiKeyID, clientIP, userAgent)
	active, err := store.IsCyberSessionScopeActive(ctx, scopeKey)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session scope read failed: err=%v", err)
		return ""
	}
	if !active {
		return ""
	}
	transcript := deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body)
	keys := transcript.lookupKeys
	if transcript.lookupKeysTruncated {
		// 粗作用域可能被许多无关终端用户共享，不能仅凭"对话超长"就拦截。
		// 只滑动窗口又会让被屏蔽会话靠追加条目逃逸，所以按命中时记录的
		// 前缀位置补查窗口外的 key。
		positions, err := store.CyberSessionScopePositions(ctx, scopeKey)
		if err != nil {
			logger.LegacyPrintf("service.openai_gateway", "cyber session scope positions read failed: err=%v", err)
		} else if len(positions) > 0 {
			transcript = deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body, positions...)
			keys = append(transcript.positionKeys, transcript.lookupKeys...)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	key, err := store.FindCyberSessionBlocked(ctx, keys)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session block batch read failed: err=%v", err)
		return ""
	}
	return key
}
