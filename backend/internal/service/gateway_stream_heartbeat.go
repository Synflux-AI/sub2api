package service

import "github.com/gin-gonic/gin"

// gatewayStreamHeartbeatBytesKey 累计 handler 在等待并发槽位期间写给客户端的
// SSE ping 字节（ConcurrencyHelper.waitForSlotWithPing / 用户消息排队）。
// 这些 ping 会提交 200 + SSE 响应头，但不是语义输出：service 判定「是否已开始
// 向客户端输出」时必须扣掉它们，否则排队超过一个 ping 间隔的请求在开流后
// 遇到可换号的错误（如流内过载）就再也无法换号（#298）。
const gatewayStreamHeartbeatBytesKey = "gateway_stream_heartbeat_bytes"

// RecordGatewayStreamHeartbeat 累加一次等槽 ping 的写出字节数。
func RecordGatewayStreamHeartbeat(c *gin.Context, written int) {
	if c == nil || written <= 0 {
		return
	}
	c.Set(gatewayStreamHeartbeatBytesKey, GatewayStreamHeartbeatBytes(c)+written)
}

// GatewayStreamHeartbeatBytes 返回本请求已写出的等槽 ping 字节数。
func GatewayStreamHeartbeatBytes(c *gin.Context) int {
	if c == nil {
		return 0
	}
	value, ok := c.Get(gatewayStreamHeartbeatBytesKey)
	if !ok {
		return 0
	}
	bytes, _ := value.(int)
	return bytes
}
