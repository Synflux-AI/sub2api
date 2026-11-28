package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// anchorTestCache 是带批量读取的内存粘性缓存，按 groupID 隔离，模拟多副本共享的 Redis。
type anchorTestCache struct {
	GatewayCache
	mu        sync.Mutex
	bindings  map[int64]map[string]int64
	batchErr  error
	gets      int
	batchGets int
	sets      int
}

func newAnchorTestCache() *anchorTestCache {
	return &anchorTestCache{bindings: map[int64]map[string]int64{}}
}

func (c *anchorTestCache) GetSessionAccountID(_ context.Context, groupID int64, sessionHash string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if id, ok := c.bindings[groupID][sessionHash]; ok {
		return id, nil
	}
	return 0, ErrStickySessionNotFound
}

func (c *anchorTestCache) GetSessionAccountIDs(_ context.Context, groupID int64, sessionHashes []string) ([]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batchGets++
	if c.batchErr != nil {
		return nil, c.batchErr
	}
	ids := make([]int64, len(sessionHashes))
	for i, h := range sessionHashes {
		ids[i] = c.bindings[groupID][h]
	}
	return ids, nil
}

func (c *anchorTestCache) SetSessionAccountID(_ context.Context, groupID int64, sessionHash string, accountID int64, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	if c.bindings[groupID] == nil {
		c.bindings[groupID] = map[string]int64{}
	}
	c.bindings[groupID][sessionHash] = accountID
	return nil
}

func (c *anchorTestCache) DeleteSessionAccountID(_ context.Context, groupID int64, sessionHash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.bindings[groupID], sessionHash)
	return nil
}

func (c *anchorTestCache) anchorKeyCount(groupID int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k := range c.bindings[groupID] {
		if strings.HasPrefix(k, anthropicStickyAnchorKeyPrefix) {
			n++
		}
	}
	return n
}

type anchorTestEnv struct {
	repo  *gatewayTTLSettingRepo
	cache *anchorTestCache
	svc   *GatewayService
}

func newAnchorTestEnv(t *testing.T, settings map[string]string) *anchorTestEnv {
	t.Helper()
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
	t.Cleanup(func() { gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{}) })
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	for k, v := range settings {
		repo.data[k] = v
	}
	cache := newAnchorTestCache()
	return &anchorTestEnv{
		repo:  repo,
		cache: cache,
		svc:   &GatewayService{cache: cache, settingService: NewSettingService(repo, &config.Config{})},
	}
}

// newReplica 返回共享同一缓存与设置的另一个 GatewayService，模拟另一个副本。
func (e *anchorTestEnv) newReplica() *GatewayService {
	return &GatewayService{cache: e.cache, settingService: e.svc.settingService}
}

func (e *anchorTestEnv) setSetting(key, value string) {
	e.repo.data[key] = value
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
}

func anchorParse(t *testing.T, system any, messages []any, metadataUserID string) *ParsedRequest {
	t.Helper()
	body := map[string]any{"model": "claude-sonnet-4.5", "max_tokens": 16, "messages": messages}
	if system != nil {
		body["system"] = system
	}
	if metadataUserID != "" {
		body["metadata"] = map[string]any{"user_id": metadataUserID}
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(raw), domain.PlatformAnthropic)
	require.NoError(t, err)
	parsed.SessionContext = &SessionContext{ClientIP: "203.0.113.1", UserAgent: "ide-agent/1.0", APIKeyID: 1}
	return parsed
}

var ephemeral = map[string]any{"type": "ephemeral"}

func userText(text string, cached bool) map[string]any {
	block := map[string]any{"type": "text", "text": text}
	if cached {
		block["cache_control"] = ephemeral
	}
	return map[string]any{"role": "user", "content": []any{block}}
}

func assistantText(text string, thinking bool) map[string]any {
	content := []any{}
	if thinking {
		content = append(content, map[string]any{"type": "thinking", "thinking": "reasoning", "signature": "sig"})
	}
	content = append(content, map[string]any{"type": "text", "text": text})
	return map[string]any{"role": "assistant", "content": content}
}

func assistantToolUse(id string) map[string]any {
	return map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "text", "text": "running tool"},
		map[string]any{"type": "tool_use", "id": id, "name": "read_file", "input": map[string]any{"path": "a.go"}},
	}}
}

func toolResult(id string, cached bool) map[string]any {
	block := map[string]any{"type": "tool_result", "tool_use_id": id, "content": "file body"}
	if cached {
		block["cache_control"] = ephemeral
	}
	return map[string]any{"role": "user", "content": []any{block}}
}

const anchorSystem = "You are a coding agent."

// serve 模拟 handler：解析锚点、选号（命中则用命中账号，否则用 pick）、成功后写最深锚点。
func serve(t *testing.T, svc *GatewayService, groupID int64, parsed *ParsedRequest, pick int64) (AnthropicStickyResolution, int64) {
	t.Helper()
	gid := groupID
	res, ok := svc.ResolveAnthropicStickyAnchor(context.Background(), &gid, parsed)
	require.True(t, ok)
	account := pick
	if res.BoundAccountID > 0 {
		account = res.BoundAccountID
	}
	require.NoError(t, svc.BindStickySession(context.Background(), &gid, res.SessionHash, account))
	return res, account
}

func TestStickyAnchor_NewTurnThenToolLoopSameAccount(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	// T1：首轮，cache_control 在首条用户消息上
	t1 := anchorParse(t, anchorSystem, []any{userText("Q1", true)}, "")
	r1, acc1 := serve(t, env.svc, 1, t1, 101)
	require.Zero(t, r1.MatchedDepth)
	require.Equal(t, int64(101), acc1)

	// A：新一轮，cache_control 挪到本轮用户消息
	a := anchorParse(t, anchorSystem, []any{userText("Q1", false), assistantText("A1", false), userText("Q2", true)}, "")
	ra, accA := serve(t, env.svc, 1, a, 202)
	require.Equal(t, 1, ra.MatchedDepth)
	require.Equal(t, int64(101), accA)

	// B：工具循环，cache_control 挪到 tool_result，前 3 条消息与 A 相同（去掉 cache_control）
	b := anchorParse(t, anchorSystem, []any{userText("Q1", false), assistantText("A1", false), userText("Q2", false), assistantToolUse("t1"), toolResult("t1", true)}, "")
	rb, accB := serve(t, env.svc, 1, b, 303)
	require.Equal(t, ra.SessionHash, rb.SessionHash, "tool loop must reuse the depth-K anchor of its turn")
	require.Equal(t, 3, rb.MatchedDepth)
	require.Equal(t, int64(101), accB)
}

func TestStickyAnchor_MultiTurnStableAndKeysBounded(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	// 第 turn 轮请求：Q1, A1, …, Q_turn。只有本轮用户消息带 cache_control，
	// 只有上一条 assistant 回复保留 thinking，更早的 thinking 已被客户端剥离。
	buildTurn := func(turn int) []any {
		msgs := make([]any, 0, 2*turn-1)
		for i := 1; i <= turn; i++ {
			if i > 1 {
				msgs = append(msgs, assistantText(fmt.Sprintf("A%d", i-1), i == turn))
			}
			msgs = append(msgs, userText(fmt.Sprintf("Q%d", i), i == turn))
		}
		return msgs
	}
	_, first := serve(t, env.svc, 1, anchorParse(t, anchorSystem, buildTurn(1), ""), 101)
	for turn := 2; turn <= 8; turn++ {
		_, acc := serve(t, env.svc, 1, anchorParse(t, anchorSystem, buildTurn(turn), ""), int64(900+turn))
		require.Equal(t, first, acc, "turn %d must stay on the first account", turn)
	}
	// 默认深度 3：只会产生 H1 与 H3 两把 key，不随轮数增长
	require.Equal(t, 2, env.cache.anchorKeyCount(1))
}

func TestStickyAnchor_ThinkingAndCacheControlNormalized(t *testing.T) {
	plain := buildAnthropicStickyAnchors([]byte(`"sys"`), []byte(`[{"role":"user","content":"Q1"},{"role":"assistant","content":[{"type":"text","text":"A1"}]},{"role":"user","content":[{"type":"text","text":"Q2"}]}]`), 3)
	noisy := buildAnthropicStickyAnchors([]byte(`[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}]`), []byte(`[{"content":[{"text":"Q1","type":"text","cache_control":{"type":"ephemeral"}}],"role":"user"},{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"},{"type":"redacted_thinking","data":"y"},{"type":"text","text":"A1"}]},{"role":"user","content":[{"type":"text","text":"Q2","cache_control":{"type":"ephemeral"}}]}]`), 3)
	require.Len(t, plain, 3)
	require.Equal(t, plain, noisy, "string vs block content, key order, cache_control and thinking must not affect anchors")
	for _, a := range plain {
		require.True(t, strings.HasPrefix(a, anthropicStickyAnchorKeyPrefix))
		require.Len(t, strings.TrimPrefix(a, anthropicStickyAnchorKeyPrefix), 32, "anchor hash must be 128 bits")
	}

	// user 消息中的 thinking 形状块不被剥离（只剥 assistant）；内容变化必须改变锚点
	changed := buildAnthropicStickyAnchors([]byte(`"sys"`), []byte(`[{"role":"user","content":"Q1 changed"}]`), 3)
	require.NotEqual(t, plain[0], changed[0])
	otherSystem := buildAnthropicStickyAnchors([]byte(`"other sys"`), []byte(`[{"role":"user","content":"Q1"}]`), 3)
	require.NotEqual(t, plain[0], otherSystem[0], "system participates in the anchor")
}

func TestStickyAnchor_DepthBoundaries(t *testing.T) {
	msgs := `[{"role":"user","content":"1"},{"role":"assistant","content":"2"},{"role":"user","content":"3"},{"role":"assistant","content":"4"},{"role":"user","content":"5"},{"role":"assistant","content":"6"},{"role":"user","content":"7"}]`
	for _, tc := range []struct {
		depth, want int
	}{{1, 1}, {3, 3}, {5, 5}, {0, 3}, {9, 3}, {-1, 3}} {
		anchors := buildAnthropicStickyAnchors(nil, []byte(msgs), tc.depth)
		require.Len(t, anchors, tc.want, "depth %d", tc.depth)
	}
	// 前缀性质：深度 5 的前 3 把锚点与深度 3 完全一致
	require.Equal(t, buildAnthropicStickyAnchors(nil, []byte(msgs), 3), buildAnthropicStickyAnchors(nil, []byte(msgs), 5)[:3])
	// 消息数少于 K 时最深锚点为 H_n
	require.Len(t, buildAnthropicStickyAnchors(nil, []byte(`[{"role":"user","content":"1"}]`), 5), 1)
	require.Nil(t, buildAnthropicStickyAnchors(nil, []byte(`[]`), 3))
	require.Nil(t, buildAnthropicStickyAnchors(nil, nil, 3))
}

func TestStickyAnchor_SettingsParsing(t *testing.T) {
	require.Equal(t, AnthropicStickyAnchorSettings{Enabled: true, Depth: 3}, parseAnthropicStickyAnchorSettings(map[string]string{}))
	require.Equal(t, AnthropicStickyAnchorSettings{Enabled: false, Depth: 5}, parseAnthropicStickyAnchorSettings(map[string]string{
		SettingKeyAnthropicStickyAnchorEnabled: "false", SettingKeyAnthropicStickyAnchorDepth: "5",
	}))
	for _, bad := range []string{"0", "9", "abc", "-2", "3.5"} {
		require.Equal(t, 3, parseAnthropicStickyAnchorSettings(map[string]string{SettingKeyAnthropicStickyAnchorDepth: bad}).Depth, bad)
	}

	// 库中无设置键时按默认开启处理
	env := newAnchorTestEnv(t, nil)
	require.Equal(t, AnthropicStickyAnchorSettings{Enabled: true, Depth: 3}, env.svc.settingService.GetAnthropicStickyAnchorSettings(context.Background()))
}

func TestStickyAnchor_SharedAPIKeyIndependentCallsDoNotStick(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	_, accA := serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("task-A", true)}, ""), 101)
	rb, accB := serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("task-B", true)}, ""), 202)
	require.Zero(t, rb.MatchedDepth, "same system but different first message must not match")
	require.NotEqual(t, accA, accB)
}

func TestStickyAnchor_SameFirstMessageSplitsFromSecondTurn(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("hello", true)}, ""), 101)
	// 会话 X 第二轮落 101（命中 H1）并写入 H3(X)
	_, accX := serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("hello", false), assistantText("hi X", false), userText("do X", true)}, ""), 0)
	require.Equal(t, int64(101), accX)
	// 会话 Y 第二轮：H3(Y) 与 H3(X) 不同
	ry, _ := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, []any{userText("hello", false), assistantText("hi Y", false), userText("do Y", true)}, ""))
	rx, _ := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, []any{userText("hello", false), assistantText("hi X", false), userText("do X", true)}, ""))
	require.NotEqual(t, rx.SessionHash, ry.SessionHash)
	require.Equal(t, 3, rx.MatchedDepth)
	require.Equal(t, 1, ry.MatchedDepth, "Y only shares the shallow anchor")

	// Y 粘性账号被否决、落到 202 后，Y 后续请求跟随 202，X 不受影响
	gid := int64(1)
	require.NoError(t, env.svc.BindStickySession(context.Background(), &gid, ry.SessionHash, 202))
	ry2, _ := env.svc.ResolveAnthropicStickyAnchor(context.Background(), &gid, anchorParse(t, anchorSystem, []any{userText("hello", false), assistantText("hi Y", false), userText("do Y", false), assistantText("ok", false), userText("more", true)}, ""))
	require.Equal(t, int64(202), ry2.BoundAccountID)
	rx2, _ := env.svc.ResolveAnthropicStickyAnchor(context.Background(), &gid, anchorParse(t, anchorSystem, []any{userText("hello", false), assistantText("hi X", false), userText("do X", true)}, ""))
	require.Equal(t, int64(101), rx2.BoundAccountID)
}

func TestStickyAnchor_ConcurrentToolCallsSameAccount(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	base := []any{userText("Q1", false), assistantText("A1", false), userText("Q2", true)}
	serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("Q1", true)}, ""), 101)
	serve(t, env.svc, 1, anchorParse(t, anchorSystem, base, ""), 0)

	var wg sync.WaitGroup
	results := make([]int64, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msgs := append(append([]any{}, base[:2]...), userText("Q2", false), assistantToolUse("t"), toolResult("t", true))
			res, ok := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, msgs, ""))
			if ok {
				results[i] = res.BoundAccountID
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, []int64{101, 101}, results)
}

func TestStickyAnchor_FollowsNewAccountAfterVeto(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("Q1", true)}, ""), 101)
	msgs := []any{userText("Q1", false), assistantText("A1", false), userText("Q2", true)}
	// 粘性账号 101 不可调度 → 实际服务账号 202，最深锚点写 202
	res, ok := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, msgs, ""))
	require.True(t, ok)
	require.Equal(t, int64(101), res.BoundAccountID)
	require.NoError(t, env.svc.BindStickySession(context.Background(), ptrInt64(1), res.SessionHash, 202))

	// 后续请求命中最深锚点 → 202，不回落 H1 的 101
	for i := 0; i < 3; i++ {
		next := append(append([]any{}, msgs...), assistantText("A2", false), userText("Q3", true))
		r, _ := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, next, ""))
		require.Equal(t, int64(202), r.BoundAccountID)
		require.Equal(t, 3, r.MatchedDepth)
	}
}

func TestStickyAnchor_MultiReplicaSharesBinding(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	replicaB := env.newReplica()
	serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("Q1", true)}, ""), 101)
	res, ok := replicaB.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, []any{userText("Q1", false), assistantText("A1", false), userText("Q2", true)}, ""))
	require.True(t, ok)
	require.Equal(t, int64(101), res.BoundAccountID)
}

func TestStickyAnchor_MetadataUserIDTakesPriority(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	meta := "user_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2_account__session_123e4567-e89b-12d3-a456-426614174000"
	parsed := anchorParse(t, anchorSystem, []any{userText("Q1", true)}, meta)
	_, ok := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), parsed)
	require.False(t, ok)
	require.Equal(t, "123e4567-e89b-12d3-a456-426614174000", env.svc.GenerateSessionHash(parsed))
	require.Zero(t, env.cache.batchGets+env.cache.gets+env.cache.sets)
}

func TestStickyAnchor_DisabledFallsBackWithoutAnchorIO(t *testing.T) {
	env := newAnchorTestEnv(t, map[string]string{SettingKeyAnthropicStickyAnchorEnabled: "false"})
	parsed := anchorParse(t, anchorSystem, []any{userText("Q1", false), assistantText("A1", false), userText("Q2", true)}, "")
	want := (&GatewayService{}).GenerateSessionHash(anchorParse(t, anchorSystem, []any{userText("Q1", false), assistantText("A1", false), userText("Q2", true)}, ""))

	_, ok := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), parsed)
	require.False(t, ok)
	require.Zero(t, env.cache.batchGets+env.cache.gets+env.cache.sets, "disabled switch must not read or write anchor keys")
	require.Equal(t, want, env.svc.GenerateSessionHash(parsed), "fallback must be byte-identical to GenerateSessionHash")
	require.False(t, strings.HasPrefix(want, anthropicStickyAnchorKeyPrefix))

	// 运行中打开开关：下一个请求即走锚点
	env.setSetting(SettingKeyAnthropicStickyAnchorEnabled, "true")
	_, ok = env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), parsed)
	require.True(t, ok)
}

func TestStickyAnchor_IncreasingDepthAtRuntimeKeepsAccount(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	msgs := []any{userText("1", false), assistantText("2", false), userText("3", false), assistantText("4", false), userText("5", true)}
	_, acc := serve(t, env.svc, 1, anchorParse(t, anchorSystem, msgs, ""), 101)
	require.Equal(t, int64(101), acc)

	env.setSetting(SettingKeyAnthropicStickyAnchorDepth, "5")
	res, acc2 := serve(t, env.svc, 1, anchorParse(t, anchorSystem, msgs, ""), 202)
	require.Equal(t, 5, res.AnchorDepth)
	require.Equal(t, 3, res.MatchedDepth, "deeper anchor misses and falls back to the previous depth")
	require.Equal(t, int64(101), acc2)
	res3, _ := serve(t, env.svc, 1, anchorParse(t, anchorSystem, msgs, ""), 303)
	require.Equal(t, 5, res3.MatchedDepth)
}

func TestStickyAnchor_RedisFailureFallsBack(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	env.cache.batchErr = errors.New("redis down")
	_, ok := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, []any{userText("Q1", true)}, ""))
	require.False(t, ok, "lookup failure must fall back to the original session hash")

	// 无缓存时同样回退
	_, ok = (&GatewayService{}).ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(1), anchorParse(t, anchorSystem, []any{userText("Q1", true)}, ""))
	require.False(t, ok)
}

func TestStickyAnchor_GroupNamespaceIsolation(t *testing.T) {
	env := newAnchorTestEnv(t, nil)
	serve(t, env.svc, 1, anchorParse(t, anchorSystem, []any{userText("Q1", true)}, ""), 101)
	res, _ := env.svc.ResolveAnthropicStickyAnchor(context.Background(), ptrInt64(2), anchorParse(t, anchorSystem, []any{userText("Q1", true)}, ""))
	require.Zero(t, res.BoundAccountID)
}
