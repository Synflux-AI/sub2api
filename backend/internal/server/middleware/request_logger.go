package middleware

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const requestIDHeader = "X-Request-ID"

// RequestLogger 在请求入口注入 request-scoped logger。
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		requestID, validRequestID := normalizeCorrelationID(c.GetHeader(requestIDHeader))
		if !validRequestID {
			requestID = uuid.NewString()
		}
		c.Header(requestIDHeader, requestID)

		ctx := context.WithValue(c.Request.Context(), ctxkey.RequestID, requestID)
		clientRequestID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
		clientRequestID, _ = normalizeCorrelationID(clientRequestID)

		// 入站 X-Trace-Id：合法则透传，非法或缺失均以 requestID 兜底；
		// 仅在“非空但非法”时记 warn，完全缺失时保持静默。
		rawTraceID := c.GetHeader(traceid.Header)
		traceID := requestID
		rejectedTraceID := false
		if trimmed := strings.TrimSpace(rawTraceID); trimmed != "" {
			if normalized, ok := traceid.Normalize(rawTraceID); ok {
				traceID = normalized
			} else {
				rejectedTraceID = true
			}
		}
		c.Header(traceid.Header, traceID)
		ctx = context.WithValue(ctx, ctxkey.TraceID, traceID)

		// 这个 base logger 会通过 ctx 流到全仓的业务日志上（logger.C / logger.CtxPrintf），
		// 因此只绑请求级关联字段，不预绑通用 component：业务侧要追加自己的 component，
		// 而 zap 的 JSON encoder 会把重复 key 逐字写出，取值就得看下游解析器实现。
		// 同理，空的 client_request_id 也不绑——按路由挂载的 ClientRequestID() 中间件
		// 会在稍后补上真值（internal/server/routes/gateway.go:38）。
		fields := []zap.Field{
			zap.String("request_id", requestID),
			zap.String("trace_id", traceID),
			zap.String("path", c.Request.URL.Path),
			zap.String("method", c.Request.Method),
		}
		if trimmed := strings.TrimSpace(clientRequestID); trimmed != "" {
			fields = append(fields, zap.String("client_request_id", trimmed))
		}
		requestLogger := logger.With(fields...)

		if rejectedTraceID {
			// 注：若原始值整体为非法 UTF-8（如 "\xff\xfe"），normalizePersistentText 会将其
			// 清空为 ""，此时诊断字段为空串——这是已知的信息损失，不影响 warn 是否触发。
			requestLogger.Warn("inbound X-Trace-Id rejected",
				zap.String("component", "http"),
				zap.String("trace_id_rejected", normalizePersistentText(rawTraceID, maxPersistentRequestIDBytes)),
			)
		}

		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
