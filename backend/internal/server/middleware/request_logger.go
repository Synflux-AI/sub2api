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

		requestLogger := logger.With(
			zap.String("component", "http"),
			zap.String("request_id", requestID),
			zap.String("client_request_id", strings.TrimSpace(clientRequestID)),
			zap.String("trace_id", traceID),
			zap.String("path", c.Request.URL.Path),
			zap.String("method", c.Request.Method),
		)

		if rejectedTraceID {
			requestLogger.Warn("inbound X-Trace-Id rejected",
				zap.String("trace_id_rejected", normalizePersistentText(rawTraceID, maxPersistentRequestIDBytes)),
			)
		}

		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
