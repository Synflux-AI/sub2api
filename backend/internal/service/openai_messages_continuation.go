package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type openAICompatSessionResponseBinding struct {
	ResponseID           string
	TurnState            string
	ContinuationDisabled bool
	ExpiresAt            time.Time
}

func openAICompatContinuationEnabled(account *Account, model string) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	return shouldAutoInjectPromptCacheKeyForCompat(model)
}

func trimAnthropicCompatResponsesInputToLatestTurn(req *apicompat.ResponsesRequest) {
	if req == nil || len(req.Input) == 0 {
		return
	}

	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil || len(items) == 0 {
		return
	}

	start := latestAnthropicCompatResponsesInputTurnStart(items)
	trimmed := append([]apicompat.ResponsesInputItem(nil), items[start:]...)
	if len(trimmed) == len(items) {
		return
	}
	if input, err := json.Marshal(trimmed); err == nil {
		req.Input = input
	}
}

func latestAnthropicCompatResponsesInputTurnStart(items []apicompat.ResponsesInputItem) int {
	if len(items) == 0 {
		return 0
	}

	start := len(items) - 1
	last := items[start]
	switch {
	case last.Type == "function_call_output":
		for start > 0 && items[start-1].Type == "function_call_output" {
			start--
		}
	case last.Type == "message" && last.Role == "user":
		for start > 0 && items[start-1].Type == "function_call_output" {
			start--
		}
	default:
		return start
	}

	return expandAnthropicCompatResponsesInputToolCallStart(items, start)
}

func expandAnthropicCompatResponsesInputToolCallStart(items []apicompat.ResponsesInputItem, start int) int {
	if start < 0 || start >= len(items) {
		return start
	}

	needed := make(map[string]struct{})
	for i := start; i < len(items); i++ {
		if items[i].Type != "function_call_output" {
			continue
		}
		callID := strings.TrimSpace(items[i].CallID)
		if callID != "" {
			needed[callID] = struct{}{}
		}
	}
	if len(needed) == 0 {
		return start
	}

	expandedStart := start
	for i := start - 1; i >= 0 && len(needed) > 0; i-- {
		if items[i].Type != "function_call" {
			continue
		}
		callID := strings.TrimSpace(items[i].CallID)
		if _, ok := needed[callID]; !ok {
			continue
		}
		delete(needed, callID)
		expandedStart = i
	}
	return expandedStart
}

func isOpenAICompatPreviousResponseNotFound(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		return strings.Contains(lower, "previous_response_not_found") ||
			lower == "previous_response_id is not available for this user" ||
			(strings.Contains(lower, "previous response") && strings.Contains(lower, "not found")) ||
			(strings.Contains(lower, "unsupported parameter") && strings.Contains(lower, "previous_response_id"))
	}
	if check(upstreamMsg) || check(string(upstreamBody)) {
		return true
	}
	return check(gjson.GetBytes(upstreamBody, "error.code").String()) ||
		check(gjson.GetBytes(upstreamBody, "error.message").String())
}

func isOpenAICompatPreviousResponseUnsupported(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		if !strings.Contains(lower, "previous_response_id") {
			return false
		}
		return strings.Contains(lower, "unsupported parameter") ||
			strings.Contains(lower, "only supported on responses websocket") ||
			strings.Contains(lower, "not supported") ||
			strings.Contains(lower, "is not available for this user") ||
			strings.Contains(lower, "requires an openai api-key account for http requests")
	}
	if check(upstreamMsg) || check(string(upstreamBody)) {
		return true
	}
	return check(gjson.GetBytes(upstreamBody, "error.code").String()) ||
		check(gjson.GetBytes(upstreamBody, "error.message").String())
}

func openAICompatSessionResponseKey(c *gin.Context, account *Account, promptCacheKey string) string {
	key := strings.TrimSpace(promptCacheKey)
	if account == nil || key == "" {
		return ""
	}
	apiKeyID := int64(0)
	if c != nil {
		apiKeyID = getAPIKeyIDFromContext(c)
	}
	return strings.Join([]string{
		strconv.FormatInt(account.ID, 10),
		strconv.FormatInt(apiKeyID, 10),
		key,
	}, "\x00")
}

// OpenAICompatSessionStateCache is an optional GatewayCache capability that
// shares /v1/messages→Responses compat continuation state across replicas.
// Without it the binding lives in process memory, so a conversation that
// alternates between replicas would resume from a stale previous_response_id
// while its input is already trimmed to the latest turn (silent context loss).
type OpenAICompatSessionStateCache interface {
	GetOpenAICompatSessionState(ctx context.Context, key string) ([]byte, error)
	SetOpenAICompatSessionState(ctx context.Context, key string, payload []byte, ttl time.Duration) error
	DeleteOpenAICompatSessionState(ctx context.Context, key string) error
}

const openAICompatSessionStateRedisTimeout = 2 * time.Second

type openAICompatSessionStatePayload struct {
	ResponseID           string    `json:"response_id,omitempty"`
	TurnState            string    `json:"turn_state,omitempty"`
	ContinuationDisabled bool      `json:"continuation_disabled,omitempty"`
	ExpiresAt            time.Time `json:"expires_at"`
}

func openAICompatSessionStateCacheKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *OpenAIGatewayService) openAICompatSessionStateCache() OpenAICompatSessionStateCache {
	if s == nil || s.cache == nil {
		return nil
	}
	cache, _ := s.cache.(OpenAICompatSessionStateCache)
	return cache
}

func openAICompatSessionStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), openAICompatSessionStateRedisTimeout)
}

// loadOpenAICompatSessionBinding returns the live (non-expired) binding. The
// shared cache is the source of truth when available; the process-local map is
// only used without it or when the shared cache errors.
func (s *OpenAIGatewayService) loadOpenAICompatSessionBinding(ctx context.Context, key string) (openAICompatSessionResponseBinding, bool) {
	if cache := s.openAICompatSessionStateCache(); cache != nil {
		cacheCtx, cancel := openAICompatSessionStateContext(ctx)
		raw, err := cache.GetOpenAICompatSessionState(cacheCtx, openAICompatSessionStateCacheKey(key))
		cancel()
		if err == nil {
			if len(raw) == 0 {
				return openAICompatSessionResponseBinding{}, false
			}
			var payload openAICompatSessionStatePayload
			if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
				s.deleteOpenAICompatSessionBinding(ctx, key)
				return openAICompatSessionResponseBinding{}, false
			}
			binding := openAICompatSessionResponseBinding(payload)
			if !binding.ExpiresAt.IsZero() && time.Now().After(binding.ExpiresAt) {
				return openAICompatSessionResponseBinding{}, false
			}
			return binding, true
		}
		logOpenAIWSModeInfo("compat_session_state_read_fail cause=%s", truncateOpenAIWSLogValue(err.Error(), openAIWSLogValueMaxLen))
	}
	raw, ok := s.openaiCompatSessionResponses.Load(key)
	if !ok {
		return openAICompatSessionResponseBinding{}, false
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok {
		s.openaiCompatSessionResponses.Delete(key)
		return openAICompatSessionResponseBinding{}, false
	}
	if !binding.ExpiresAt.IsZero() && time.Now().After(binding.ExpiresAt) {
		s.openaiCompatSessionResponses.Delete(key)
		return openAICompatSessionResponseBinding{}, false
	}
	return binding, true
}

func (s *OpenAIGatewayService) storeOpenAICompatSessionBinding(ctx context.Context, key string, binding openAICompatSessionResponseBinding) {
	if cache := s.openAICompatSessionStateCache(); cache != nil {
		ttl := time.Until(binding.ExpiresAt)
		if binding.ExpiresAt.IsZero() || ttl <= 0 {
			ttl = s.openAIWSResponseStickyTTL()
		}
		raw, err := json.Marshal(openAICompatSessionStatePayload(binding))
		if err == nil {
			cacheCtx, cancel := openAICompatSessionStateContext(ctx)
			err = cache.SetOpenAICompatSessionState(cacheCtx, openAICompatSessionStateCacheKey(key), raw, ttl)
			cancel()
		}
		if err == nil {
			s.openaiCompatSessionResponses.Delete(key)
			return
		}
		logOpenAIWSModeInfo("compat_session_state_write_fail cause=%s", truncateOpenAIWSLogValue(err.Error(), openAIWSLogValueMaxLen))
	}
	s.openaiCompatSessionResponses.Store(key, binding)
}

func (s *OpenAIGatewayService) deleteOpenAICompatSessionBinding(ctx context.Context, key string) {
	s.openaiCompatSessionResponses.Delete(key)
	if cache := s.openAICompatSessionStateCache(); cache != nil {
		cacheCtx, cancel := openAICompatSessionStateContext(ctx)
		err := cache.DeleteOpenAICompatSessionState(cacheCtx, openAICompatSessionStateCacheKey(key))
		cancel()
		if err != nil {
			logOpenAIWSModeInfo("compat_session_state_delete_fail cause=%s", truncateOpenAIWSLogValue(err.Error(), openAIWSLogValueMaxLen))
		}
	}
}

func (s *OpenAIGatewayService) getOpenAICompatSessionResponseID(ctx context.Context, c *gin.Context, account *Account, promptCacheKey string) string {
	if s == nil {
		return ""
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return ""
	}
	binding, ok := s.loadOpenAICompatSessionBinding(ctx, key)
	if !ok || binding.ContinuationDisabled {
		return ""
	}
	return strings.TrimSpace(binding.ResponseID)
}

func (s *OpenAIGatewayService) bindOpenAICompatSessionResponseID(ctx context.Context, c *gin.Context, account *Account, promptCacheKey, responseID string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	id := strings.TrimSpace(responseID)
	if key == "" || id == "" {
		return
	}
	binding := openAICompatSessionResponseBinding{
		ResponseID: id,
		ExpiresAt:  time.Now().Add(s.openAIWSResponseStickyTTL()),
	}
	if existing, ok := s.loadOpenAICompatSessionBinding(ctx, key); ok {
		if existing.ContinuationDisabled {
			existing.ResponseID = ""
			existing.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
			s.storeOpenAICompatSessionBinding(ctx, key, existing)
			return
		}
		binding.TurnState = existing.TurnState
	}
	s.storeOpenAICompatSessionBinding(ctx, key, binding)
}

func (s *OpenAIGatewayService) deleteOpenAICompatSessionResponseID(ctx context.Context, c *gin.Context, account *Account, promptCacheKey string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return
	}
	binding, ok := s.loadOpenAICompatSessionBinding(ctx, key)
	if !ok {
		return
	}
	binding.ResponseID = ""
	if strings.TrimSpace(binding.TurnState) == "" && !binding.ContinuationDisabled {
		s.deleteOpenAICompatSessionBinding(ctx, key)
		return
	}
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	s.storeOpenAICompatSessionBinding(ctx, key, binding)
}

func (s *OpenAIGatewayService) disableOpenAICompatSessionContinuation(ctx context.Context, c *gin.Context, account *Account, promptCacheKey string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return
	}
	binding := openAICompatSessionResponseBinding{
		ContinuationDisabled: true,
		ExpiresAt:            time.Now().Add(s.openAIWSResponseStickyTTL()),
	}
	if existing, ok := s.loadOpenAICompatSessionBinding(ctx, key); ok {
		binding.TurnState = existing.TurnState
	}
	s.storeOpenAICompatSessionBinding(ctx, key, binding)
}

func (s *OpenAIGatewayService) isOpenAICompatSessionContinuationDisabled(ctx context.Context, c *gin.Context, account *Account, promptCacheKey string) bool {
	if s == nil {
		return false
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return false
	}
	binding, ok := s.loadOpenAICompatSessionBinding(ctx, key)
	return ok && binding.ContinuationDisabled
}

func (s *OpenAIGatewayService) getOpenAICompatSessionTurnState(ctx context.Context, c *gin.Context, account *Account, promptCacheKey string) string {
	if s == nil {
		return ""
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return ""
	}
	binding, ok := s.loadOpenAICompatSessionBinding(ctx, key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(binding.TurnState)
}

func (s *OpenAIGatewayService) bindOpenAICompatSessionTurnState(ctx context.Context, c *gin.Context, account *Account, promptCacheKey, turnState string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	state := strings.TrimSpace(turnState)
	if key == "" || state == "" {
		return
	}
	binding := openAICompatSessionResponseBinding{
		TurnState: state,
		ExpiresAt: time.Now().Add(s.openAIWSResponseStickyTTL()),
	}
	if existing, ok := s.loadOpenAICompatSessionBinding(ctx, key); ok {
		binding.ResponseID = existing.ResponseID
		binding.ContinuationDisabled = existing.ContinuationDisabled
	}
	s.storeOpenAICompatSessionBinding(ctx, key, binding)
}
