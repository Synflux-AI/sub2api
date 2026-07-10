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

const clientRequestIDHeader = "X-Client-Request-ID"

// clientRequestIDMaxLen 限制沿用入站 client_request_id 的最大长度。
// 与 usage_logs.client_request_id 的 MaxLen(64) 落库约束一致，避免超长值撑爆列。
const clientRequestIDMaxLen = 64

// ClientRequestID ensures every request has a unique client_request_id in request.Context().
//
// This is used by the Ops monitoring module for end-to-end request correlation.
//
// 跨实例关联：级联部署下，上游实例通过“自定义出站请求头”把本值以
// X-Client-Request-ID 注入到指向下一级 sub2api 的请求上；下游实例在此
// 无条件沿用该入站头（经校验后），使同一关联键贯穿整条调用链。
// 附带地，Claude Code CLI 本身每请求就发送 x-client-request-id，沿用后
// 关联链条可一直延伸到终端用户的客户端。
func ClientRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		// 已由更上游中间件写入 context 时直接复用（幂等）。
		if v, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(v) != "" {
			c.Header(clientRequestIDHeader, strings.TrimSpace(v))
			c.Next()
			return
		}

		// 入站沿用：读客户端/上游实例带来的 X-Client-Request-ID。
		// 该头是客户端可控的，必须校验后才沿用——非法值（含控制字符、超长、
		// 或非受限字符集）一律丢弃并回落到新生成，避免日志注入与落库溢出。
		id := sanitizeInboundClientRequestID(c.GetHeader(clientRequestIDHeader))
		inbound := id != ""
		if !inbound {
			id = uuid.New().String()
		}

		c.Header(clientRequestIDHeader, id)
		ctx := context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, id)
		requestLogger := logger.FromContext(ctx).With(zap.String("client_request_id", id))
		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// sanitizeInboundClientRequestID 校验并规整入站的 client_request_id。
//
// 接受两类值：
//   - 合法 UUID（大小写不敏感，输出小写标准形式）；
//   - 长度 1..=64 的受限字符集：ASCII 字母数字与 '-' '_' '.'。
//
// 其余情形（空、超长、含其他字符）返回空字符串，表示不应沿用。
func sanitizeInboundClientRequestID(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if parsed, err := uuid.Parse(v); err == nil {
		return parsed.String()
	}
	if len(v) > clientRequestIDMaxLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if !isAllowedClientRequestIDByte(v[i]) {
			return ""
		}
	}
	return v
}

func isAllowedClientRequestIDByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '-' || b == '_' || b == '.':
		return true
	default:
		return false
	}
}
