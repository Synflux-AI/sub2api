package middleware

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
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

		requestID := strings.TrimSpace(c.GetHeader(requestIDHeader))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		c.Header(requestIDHeader, requestID)

		ctx := context.WithValue(c.Request.Context(), ctxkey.RequestID, requestID)

		// 不在此处注入 client_request_id：它由下游 ClientRequestID 中间件在
		// 生成/沿用后追加。若这里先以空值注入，会与之构成重复的 zap key。
		requestLogger := logger.With(
			zap.String("component", "http"),
			zap.String("request_id", requestID),
			zap.String("path", c.Request.URL.Path),
			zap.String("method", c.Request.Method),
		)

		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
