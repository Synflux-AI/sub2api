package handler

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// balanceSchemaVersion 是 /api/v1/open/balance 响应结构的版本号；字段语义发生
// 不兼容变化时递增，客户端据此判断是否需要升级解析逻辑。
const balanceSchemaVersion = 2

// balanceCurrency 目前恒为 USD：仓内余额（users.balance / frozen_balance）与计费
// 都是美元计价，没有多币种概念。字段仍显式出现在契约里，避免客户端把裸数字当
// 成本地货币。
const balanceCurrency = "USD"

// balanceRoundScale 把余额四舍五入到 4 位小数。float64 直出 JSON 会产生
// 123.45670000000001 之类的噪声，而客户契约样例是 4 位小数；仓内 /v1/usage 直出
// 未舍入的 float64，没有可继承的约定，故本接口显式定契约。
const balanceRoundScale = 10000

// balanceResponse 是客户面余额响应：**无信封扁平结构**，与面板信封
// （response.Success 的 {code,message,data}）刻意不同，照
// gateway_key_billing.go 的 keyBillingInfoResponse。面向第三方客户的 Open API
// 响应必须自描述（object + schema_version），不该套一层面板内部的信封。
type balanceResponse struct {
	Object        string    `json:"object"`
	SchemaVersion int       `json:"schema_version"`
	Currency      string    `json:"currency"`
	Balance       float64   `json:"balance"`
	FrozenBalance float64   `json:"frozen_balance"`
	ObservedAt    time.Time `json:"observed_at"`
}

// openBalanceCache 是本 handler 对 *service.BillingCacheService 的窄依赖。
// **必须**走 BillingCacheService.GetUserBalance 而不是直读 DB：这是计费闸门
// checkBalanceEligibility 看到的同一个值，客户查到的余额与实际能否发起请求才一致。
type openBalanceCache interface {
	GetUserBalance(ctx context.Context, userID int64) (float64, error)
}

// openBalanceUserReader 是取 FrozenBalance 用的窄依赖，由 *service.UserService 满足。
type openBalanceUserReader interface {
	GetByID(ctx context.Context, id int64) (*service.User, error)
}

// openBalanceTokenToucher 是 last_used_at 落库的窄依赖，由
// *service.UserAccessTokenService 满足（其内部已做防抖 + 有界队列 + 后台落库）。
type openBalanceTokenToucher interface {
	TouchLastUsed(ctx context.Context, userID int64, token string)
}

// OpenBalanceHandler 提供客户面余额查询（用户级 access token 鉴权）。
type OpenBalanceHandler struct {
	billingCache openBalanceCache
	users        openBalanceUserReader
	accessTokens openBalanceTokenToucher
}

// NewOpenBalanceHandler 构造余额 handler。三个依赖都显式判空后再装进接口：把
// nil 具体指针装进接口会得到非 nil 接口值，调用即 panic。
func NewOpenBalanceHandler(
	billingCacheService *service.BillingCacheService,
	userService *service.UserService,
	accessTokenService *service.UserAccessTokenService,
) *OpenBalanceHandler {
	h := &OpenBalanceHandler{}
	if billingCacheService != nil {
		h.billingCache = billingCacheService
	}
	if userService != nil {
		h.users = userService
	}
	if accessTokenService != nil {
		h.accessTokens = accessTokenService
	}
	return h
}

// GetBalance 返回当前令牌所属用户的余额快照。
// GET /api/v1/open/balance
//
// 错误响应沿用 middleware.AbortWithError 的 {code,message} 形状——同一个路由组的
// 401（认证失败）与 429（限流）都由中间件以该形状产出，handler 自己的失败不该
// 换成另一套信封。
func (h *OpenBalanceHandler) GetBalance(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		// 认证中间件缺位时的防御分支：与中间件的失败响应字节级一致。
		middleware2.AbortWithError(c, http.StatusUnauthorized, "INVALID_ACCESS_TOKEN", "invalid access token")
		return
	}
	ctx := c.Request.Context()
	if h.billingCache == nil || h.users == nil {
		logger.C(ctx).Warn("open_balance_dependencies_missing",
			zap.Int64("user_id", subject.UserID),
			zap.Bool("billing_cache_missing", h.billingCache == nil),
			zap.Bool("user_reader_missing", h.users == nil),
		)
		abortBalanceUnavailable(c)
		return
	}

	balance, err := h.billingCache.GetUserBalance(ctx, subject.UserID)
	if err != nil {
		// 客户拿到的是一句恒定文案，运维侧的可辨识度全靠这条日志：
		// AbortWithError 不打日志、RequestLogger 也不产出 per-request 访问行，
		// 不记的话「Redis 与 DB 双挂」和「用户行消失」在线上完全无法区分。
		// 刻意不记令牌明文。
		logger.C(ctx).Warn("open_balance_read_failed",
			zap.Int64("user_id", subject.UserID),
			zap.Error(err),
		)
		// 读取失败时刻意不 touch last_used_at（§15.2）：那个字段的语义是「这枚
		// 令牌成功取到过数据」，失败请求不该把它推新。
		abortBalanceUnavailable(c)
		return
	}
	user, err := h.users.GetByID(ctx, subject.UserID)
	if err != nil || user == nil {
		// 与上面分开记：冻结余额来自用户读取，两条失败路径的排查方向完全不同。
		logger.C(ctx).Warn("open_balance_user_read_failed",
			zap.Int64("user_id", subject.UserID),
			zap.Bool("user_missing", user == nil),
			zap.Error(err),
		)
		abortBalanceUnavailable(c)
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, balanceResponse{
		Object:        "balance",
		SchemaVersion: balanceSchemaVersion,
		Currency:      balanceCurrency,
		Balance:       roundBalanceAmount(balance),
		FrozenBalance: roundBalanceAmount(user.FrozenBalance),
		ObservedAt:    timezone.Now().UTC(),
	})

	// 限流已在中间件里通过（429 根本进不到这里），余额也读到了，此刻才落
	// last_used_at。令牌明文取自认证中间件写入的 context 键，与 user_id 组成
	// (user_id, token) 双匹配：轮换过的旧令牌不会把新令牌误标记为已使用。
	if h.accessTokens == nil {
		return
	}
	if token, hasToken := middleware2.GetUserAccessTokenPlaintextFromContext(c); hasToken {
		h.accessTokens.TouchLastUsed(ctx, subject.UserID, token)
	}
}

func abortBalanceUnavailable(c *gin.Context) {
	middleware2.AbortWithError(c, http.StatusInternalServerError, "BALANCE_UNAVAILABLE", "balance is temporarily unavailable")
}

func roundBalanceAmount(value float64) float64 {
	return math.Round(value*balanceRoundScale) / balanceRoundScale
}
