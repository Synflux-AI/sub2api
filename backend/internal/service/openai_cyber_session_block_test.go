package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCyberBlockTestCtx(headers map[string]string, body string) (*gin.Context, []byte) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	return c, []byte(body)
}

func TestCyberSessionExplicitBlockKey(t *testing.T) {
	c1, b1 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	k1 := CyberSessionExplicitBlockKey(101, c1, b1)
	require.NotEmpty(t, k1)

	// Same session, different apiKey → different key (isolation).
	c2, b2 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	require.NotEqual(t, k1, CyberSessionExplicitBlockKey(202, c2, b2))

	// Same session + same apiKey → stable key.
	c3, b3 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	require.Equal(t, k1, CyberSessionExplicitBlockKey(101, c3, b3))

	// prompt_cache_key in body counts as explicit.
	c4, b4 := newCyberBlockTestCtx(nil, `{"prompt_cache_key":"pck-1"}`)
	require.NotEmpty(t, CyberSessionExplicitBlockKey(101, c4, b4))

	// No explicit signal → empty key → caller must skip blocking entirely.
	c5, b5 := newCyberBlockTestCtx(nil, `{"input":"hello world"}`)
	require.Empty(t, CyberSessionExplicitBlockKey(101, c5, b5))

	// conversation_id header counts as explicit; key is stable and non-empty.
	c6, b6 := newCyberBlockTestCtx(map[string]string{"conversation_id": "conv-xyz"}, `{}`)
	k6 := CyberSessionExplicitBlockKey(101, c6, b6)
	require.NotEmpty(t, k6)
	c6b, b6b := newCyberBlockTestCtx(map[string]string{"conversation_id": "conv-xyz"}, `{}`)
	require.Equal(t, k6, CyberSessionExplicitBlockKey(101, c6b, b6b), "conversation_id key must be stable")
}

func TestCyberTranscriptBlockKeysRequireModelGeneratedHistory(t *testing.T) {
	first := []byte(`{"instructions":"shared","input":[{"role":"user","content":"fixed environment"},{"role":"user","content":"question one"}]}`)
	second := []byte(`{"instructions":"shared","input":[{"role":"user","content":"fixed environment"},{"role":"user","content":"question two"}]}`)
	firstKeys := CyberSessionTranscriptBlockKeys(77, first)
	secondKeys := CyberSessionTranscriptBlockKeys(77, second)
	require.Len(t, firstKeys, 1)
	require.Len(t, secondKeys, 1)
	require.NotEqual(t, firstKeys[0], secondKeys[0])

	hit := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	continuation := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"different trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	hitKeys := CyberSessionTranscriptBlockKeys(77, hit)
	require.Len(t, hitKeys, 2)
	require.Contains(t, CyberSessionTranscriptLookupKeys(77, continuation), hitKeys[1])
}

func TestCyberTranscriptBlockKeysWebSocketResponseCreate(t *testing.T) {
	body := []byte(`{"type":"response.create","response":{"prompt_cache_key":"ws-session","input":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}}`)
	c, _ := newCyberBlockTestCtx(nil, string(body))
	require.NotEmpty(t, CyberSessionExplicitBlockKey(88, c, body))
	require.Len(t, CyberSessionTranscriptBlockKeys(88, body), 2)
}

func TestCyberTranscriptLookupKeysAreBoundedAndKeepNewestOrder(t *testing.T) {
	messages := make([]map[string]string, maxOpenAICyberTranscriptLookupKeys+44)
	for i := range messages {
		messages[i] = map[string]string{"role": "user", "content": "message-" + strconv.Itoa(i)}
	}
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)

	keys := CyberSessionTranscriptLookupKeys(77, body)
	require.Len(t, keys, maxOpenAICyberTranscriptLookupKeys)

	firstRetainedBody, err := json.Marshal(map[string]any{"messages": messages[:45]})
	require.NoError(t, err)
	firstRetainedPrefix := CyberSessionTranscriptLookupKeys(77, firstRetainedBody)
	require.Equal(t, firstRetainedPrefix[len(firstRetainedPrefix)-1], keys[0])

	fullKey := CyberSessionTranscriptBlockKeys(77, body)[0]
	require.Equal(t, fullKey, keys[len(keys)-1])
}

// --- fakes ---

type fakeCyberBlockStore struct {
	blocked   map[string]bool
	scopes    map[string]bool
	positions map[string][]int
	findCalls int
	findKeys  [][]string
}

var _ CyberSessionBlockStore = (*fakeCyberBlockStore)(nil)

func (f *fakeCyberBlockStore) SetCyberSessionBlocked(_ context.Context, scopeKey string, keys []string, _ time.Duration) error {
	if f.blocked == nil {
		f.blocked = map[string]bool{}
	}
	for _, key := range keys {
		f.blocked[key] = true
	}
	if scopeKey != "" {
		if f.scopes == nil {
			f.scopes = map[string]bool{}
		}
		f.scopes[scopeKey] = true
	}
	return nil
}

func (f *fakeCyberBlockStore) IsCyberSessionScopeActive(_ context.Context, scopeKey string) (bool, error) {
	return f.scopes[scopeKey], nil
}

func (f *fakeCyberBlockStore) AddCyberSessionScopePositions(_ context.Context, scopeKey string, positions []int, _ time.Duration) error {
	if f.positions == nil {
		f.positions = map[string][]int{}
	}
	f.positions[scopeKey] = append(f.positions[scopeKey], positions...)
	return nil
}

func (f *fakeCyberBlockStore) CyberSessionScopePositions(_ context.Context, scopeKey string) ([]int, error) {
	return f.positions[scopeKey], nil
}

func (f *fakeCyberBlockStore) FindCyberSessionBlocked(_ context.Context, keys []string) (string, error) {
	f.findCalls++
	f.findKeys = append(f.findKeys, append([]string(nil), keys...))
	for _, key := range keys {
		if f.blocked[key] {
			return key, nil
		}
	}
	return "", nil
}

// fakeSettingRepo is a minimal SettingRepository stub for unit tests.
// Only GetValue is exercised by GetCyberSessionBlockRuntime; all other methods
// panic so accidental calls are caught immediately.
type fakeSettingRepo struct {
	vals map[string]string
}

func (r *fakeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := r.vals[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}
func (r *fakeSettingRepo) Get(_ context.Context, _ string) (*Setting, error) {
	panic("fakeSettingRepo.Get not implemented")
}
func (r *fakeSettingRepo) Set(_ context.Context, _, _ string) error {
	panic("fakeSettingRepo.Set not implemented")
}
func (r *fakeSettingRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	panic("fakeSettingRepo.GetMultiple not implemented")
}
func (r *fakeSettingRepo) SetMultiple(_ context.Context, _ map[string]string) error {
	panic("fakeSettingRepo.SetMultiple not implemented")
}
func (r *fakeSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	panic("fakeSettingRepo.GetAll not implemented")
}
func (r *fakeSettingRepo) Delete(_ context.Context, _ string) error {
	panic("fakeSettingRepo.Delete not implemented")
}

var _ SettingRepository = (*fakeSettingRepo)(nil)

// comboCacheAndStore implements both GatewayCache (no-op stubs) and
// CyberSessionBlockStore (delegates to fakeCyberBlockStore) so it can be
// injected as s.cache and successfully type-asserted to CyberSessionBlockStore.
type comboCacheAndStore struct {
	store fakeCyberBlockStore
}

var _ GatewayCache = (*comboCacheAndStore)(nil)
var _ CyberSessionBlockStore = (*comboCacheAndStore)(nil)

func (c *comboCacheAndStore) GetSessionAccountID(_ context.Context, _ int64, _ string) (int64, error) {
	return 0, errors.New("stub")
}
func (c *comboCacheAndStore) SetSessionAccountID(_ context.Context, _ int64, _ string, _ int64, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) RefreshSessionTTL(_ context.Context, _ int64, _ string, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) DeleteSessionAccountID(_ context.Context, _ int64, _ string) error {
	return nil
}

func (c *comboCacheAndStore) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) GetGrokVideoPendingBilling(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (c *comboCacheAndStore) ClaimGrokVideoBilled(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *comboCacheAndStore) ReleaseGrokVideoBilled(_ context.Context, _ string) error {
	return nil
}

func (c *comboCacheAndStore) SetReasoningContent(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) GetReasoningContent(_ context.Context, _ string) (string, error) {
	return "", ErrReasoningContentNotFound
}

func (c *comboCacheAndStore) SetCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string, ttl time.Duration) error {
	return c.store.SetCyberSessionBlocked(ctx, scopeKey, keys, ttl)
}
func (c *comboCacheAndStore) IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error) {
	return c.store.IsCyberSessionScopeActive(ctx, scopeKey)
}
func (c *comboCacheAndStore) AddCyberSessionScopePositions(ctx context.Context, scopeKey string, positions []int, ttl time.Duration) error {
	return c.store.AddCyberSessionScopePositions(ctx, scopeKey, positions, ttl)
}
func (c *comboCacheAndStore) CyberSessionScopePositions(ctx context.Context, scopeKey string) ([]int, error) {
	return c.store.CyberSessionScopePositions(ctx, scopeKey)
}
func (c *comboCacheAndStore) FindCyberSessionBlocked(ctx context.Context, keys []string) (string, error) {
	return c.store.FindCyberSessionBlocked(ctx, keys)
}

// --- tests ---

// TestIsCyberSessionBlocked_EmptyKeyAndNilService covers the fail-open paths:
// empty key, nil service, store missing → always false / no panic.
func TestFindCyberSessionBlocked_EmptyAndNilService(t *testing.T) {
	var nilSvc *OpenAIGatewayService
	require.Empty(t, nilSvc.FindCyberSessionBlockedForRequest(context.Background(), 1, nil, nil, "", ""))
	require.NotPanics(t, func() { nilSvc.MarkCyberSessionBlocked(context.Background(), "", []string{"k"}, nil) })

	svc := &OpenAIGatewayService{}
	require.Empty(t, svc.FindCyberSessionBlockedForRequest(context.Background(), 1, nil, nil, "", ""))
}

// TestCyberSessionBlock_RoundTrip exercises the type-assertion success path:
// mark a session blocked via a combo cache+store, then confirm IsCyberSessionBlocked
// returns true, and an unrelated key returns false.
func TestCyberSessionBlock_RoundTrip(t *testing.T) {
	// SettingService with only settingRepo set — GetCyberSessionBlockRuntime needs
	// nothing else (cfg/proxyRepo/etc. are not touched by this code path).
	settingSvc := &SettingService{
		settingRepo: &fakeSettingRepo{
			vals: map[string]string{
				SettingKeyCyberSessionBlockEnabled:    "true",
				SettingKeyCyberSessionBlockTTLSeconds: "60",
			},
		},
	}

	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{
		cache:          combo,
		settingService: settingSvc,
	}

	ctx := context.Background()
	const testKey = "deadbeef1234"

	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "sess-roundtrip"}, `{}`)
	explicitKey := CyberSessionExplicitBlockKey(1, c, body)
	require.Empty(t, svc.FindCyberSessionBlockedForRequest(ctx, 1, c, body, "203.0.113.1", "client/1.0"))

	svc.MarkCyberSessionBlocked(ctx, "", []string{explicitKey, testKey}, nil)

	require.Equal(t, explicitKey, svc.FindCyberSessionBlockedForRequest(ctx, 1, c, body, "203.0.113.1", "client/1.0"))
}

func TestFindCyberSessionBlockedForRequestUsesScopeForTranscript(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled:    "true",
		SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()

	hitBody := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	nextBody := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"different trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	nextCtx, _ := newCyberBlockTestCtx(nil, string(nextBody))
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"
	blockKey := CyberSessionTranscriptBlockKeys(9, hitBody)[1]

	// Without an active source scope, transcript candidates are never blocks.
	require.Empty(t, svc.FindCyberSessionBlockedForRequest(ctx, 9, nextCtx, nextBody, clientIP, userAgent))
	scopeKey := CyberSessionScopeKey(9, clientIP, userAgent)
	svc.MarkCyberSessionBlocked(ctx, scopeKey, []string{blockKey}, nil)
	require.Equal(t, blockKey, svc.FindCyberSessionBlockedForRequest(ctx, 9, nextCtx, nextBody, clientIP, "Codex CLI 1.2.4"))
}

func newCyberOverflowTestService() (*OpenAIGatewayService, *comboCacheAndStore) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled:    "true",
		SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	return &OpenAIGatewayService{cache: combo, settingService: settingSvc}, combo
}

// cyberLongTranscript 生成 n 条交替的 user/assistant 消息，tag 区分不同会话。
func cyberLongTranscript(t *testing.T, tag string, n int) []map[string]string {
	t.Helper()
	messages := make([]map[string]string, n)
	for i := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages[i] = map[string]string{"role": role, "content": tag + "-" + strconv.Itoa(i)}
	}
	return messages
}

func cyberMessagesBody(t *testing.T, messages []map[string]string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)
	return body
}

// 共享粗作用域（同 Key、同出口 IP、同款客户端）下，会话 A 命中后，不相关的
// 超长会话 B 不应被拦截（#323）。
func TestFindCyberSessionBlockedForRequestDoesNotBlockUnrelatedOverflowTranscript(t *testing.T) {
	svc, combo := newCyberOverflowTestService()
	ctx := context.Background()
	const apiKeyID = int64(9)
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"
	scopeKey := CyberSessionScopeKey(apiKeyID, clientIP, userAgent)

	hitBody := cyberMessagesBody(t, cyberLongTranscript(t, "session-a", 11))
	keys, positions := CyberSessionTranscriptBlockKeysWithPositions(apiKeyID, hitBody)
	svc.MarkCyberSessionBlocked(ctx, scopeKey, keys, positions)

	otherBody := cyberMessagesBody(t, cyberLongTranscript(t, "session-b", maxOpenAICyberTranscriptLookupKeys+40))
	c, _ := newCyberBlockTestCtx(nil, string(otherBody))
	require.Empty(t, svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, otherBody, clientIP, userAgent))
	require.Equal(t, 1, combo.store.findCalls)
	require.Len(t, combo.store.findKeys[0], maxOpenAICyberTranscriptLookupKeys+len(positions),
		"overflow lookup stays bounded: window plus recorded positions")
}

// 被屏蔽的会话追加任意多条目（超出窗口）后仍应被拦截。
func TestFindCyberSessionBlockedForRequestBlocksOverflowContinuationByPosition(t *testing.T) {
	svc, _ := newCyberOverflowTestService()
	ctx := context.Background()
	const apiKeyID = int64(9)
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"
	scopeKey := CyberSessionScopeKey(apiKeyID, clientIP, userAgent)

	transcript := cyberLongTranscript(t, "session-a", maxOpenAICyberTranscriptLookupKeys*3)
	hitBody := cyberMessagesBody(t, transcript[:11])
	keys, positions := CyberSessionTranscriptBlockKeysWithPositions(apiKeyID, hitBody)
	require.Equal(t, []int{11, 10}, positions)
	svc.MarkCyberSessionBlocked(ctx, scopeKey, keys, positions)

	continuation := cyberMessagesBody(t, transcript)
	c, _ := newCyberBlockTestCtx(nil, string(continuation))
	matched := svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, continuation, clientIP, userAgent)
	require.Contains(t, keys, matched)

	// 没有激活作用域时，超长对话不做 transcript 匹配。
	require.Empty(t, svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, continuation, "203.0.113.21", userAgent))
}

// 旧版本写入的作用域没有位置记录：超长对话只按窗口匹配，不再整体拦截。
func TestFindCyberSessionBlockedForRequestOverflowWithoutPositionsUsesWindow(t *testing.T) {
	svc, combo := newCyberOverflowTestService()
	ctx := context.Background()
	const apiKeyID = int64(9)
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"
	combo.store.scopes = map[string]bool{CyberSessionScopeKey(apiKeyID, clientIP, userAgent): true}

	body := cyberMessagesBody(t, cyberLongTranscript(t, "session-b", maxOpenAICyberTranscriptLookupKeys+1))
	c, _ := newCyberBlockTestCtx(nil, string(body))
	require.Empty(t, svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, body, clientIP, userAgent))
	require.Len(t, combo.store.findKeys[0], maxOpenAICyberTranscriptLookupKeys)
}

func TestCyberTranscriptPositionKeysOnlyCoverPositionsOutsideWindow(t *testing.T) {
	messages := cyberLongTranscript(t, "s", maxOpenAICyberTranscriptLookupKeys+20)
	body := cyberMessagesBody(t, messages)
	derived := deriveOpenAICyberTranscriptBlockKeys(5, body, 3, 3, 21, 200, len(messages)+5)
	require.Equal(t, len(messages), derived.itemCount)
	// 窗口覆盖位置 21..276：只有位置 3 需要补查，重复位置去重，越界位置忽略。
	require.Len(t, derived.positionKeys, 1)
	prefix := CyberSessionTranscriptLookupKeys(5, cyberMessagesBody(t, messages[:3]))
	require.Equal(t, prefix[len(prefix)-1], derived.positionKeys[0])
}

func TestCyberSessionScopeKeyNormalizesUserAgentVersion(t *testing.T) {
	base := CyberSessionScopeKey(7, "203.0.113.10", "Codex CLI 1.2.3")
	require.NotEmpty(t, base)
	require.Equal(t, base, CyberSessionScopeKey(7, "203.0.113.10", "Codex CLI 1.2.4"))
	require.NotEqual(t, base, CyberSessionScopeKey(8, "203.0.113.10", "Codex CLI 1.2.3"))
	require.NotEqual(t, base, CyberSessionScopeKey(7, "203.0.113.11", "Codex CLI 1.2.3"))
}
