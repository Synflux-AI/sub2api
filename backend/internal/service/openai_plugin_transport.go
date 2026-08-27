package service

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// timedDoOpenAIUpstream 是 doOpenAIUpstream 加「把本次上游耗时落进 gin.Context」。
//
// 对齐 Anthropic 侧的 timedGatewayUpstreamDoWithTLS。加它是因为错误处理规则的
// 「上游耗时上限」条件读的就是 OpsUpstreamLatencyMsKey，而耗时未知是 fail-closed
// （不满足任何已配置的阈值）—— 没插桩的路径上，那个界面开关会永远不生效。
//
// 与 Anthropic 侧同样的口径说明：一次请求内的多次上游尝试会互相覆盖，最终留下最后
// 一次的数字；流式路径上这个值是「拿到响应头」为止的耗时（TTFB），不是整段流的时长。
func (s *OpenAIGatewayService) timedDoOpenAIUpstream(c *gin.Context, request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	upstreamStart := time.Now()
	resp, err := s.doOpenAIUpstream(request, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	return resp, err
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
