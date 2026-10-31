package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	preOutputStageCreated    = `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}`
	preOutputStageInProgress = `{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_1","status":"in_progress"}}`
	preOutputStageReasoning  = `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"gAAAAAB-ciphertext","summary":[]}}`
	preOutputStageUnknown    = `{"type":"response.unrecognized_progress","sequence_number":2}`
	preOutputStageOverload   = `{"type":"error","sequence_number":3,"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`
	preOutputStageFailed     = `{"type":"response.failed","sequence_number":4,"response":{"id":"resp_1","status":"failed","error":{"code":"server_error","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}}`
	preOutputStageDelta      = `{"type":"response.output_text.delta","sequence_number":3,"output_index":1,"content_index":0,"delta":"hello"}`
	preOutputStageCompleted  = `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`
)

func preOutputStageSSE(payloads ...string) string {
	var b strings.Builder
	for _, payload := range payloads {
		_, _ = b.WriteString("event: " + effectiveOpenAISSEEventType([]byte(payload), "") + "\n")
		_, _ = b.WriteString("data: " + payload + "\n\n")
	}
	return b.String()
}

func useOpenAIPreOutputStaging(t *testing.T, cfg OpenAIPreOutputStagingSettings) {
	t.Helper()
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{
		openAITTFTMode:         OpenAITTFTModeSemantic,
		openAIPreOutputStaging: cfg,
		expiresAt:              time.Now().Add(time.Minute).UnixNano(),
	})
	t.Cleanup(func() {
		gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: OpenAITTFTModeSemantic, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	})
}

func enabledPreOutputStaging() OpenAIPreOutputStagingSettings {
	cfg := defaultOpenAIPreOutputStagingSettings()
	cfg.Enabled = true
	return cfg
}

type preOutputStageRun struct {
	err  error
	body string
	c    *gin.Context
}

func runPreOutputStageStream(t *testing.T, path string, gateway config.GatewayConfig, body io.ReadCloser) preOutputStageRun {
	t.Helper()
	return runPreOutputStageStreamWith(t, path, gateway, body, nil)
}

// runPreOutputStageStreamWith 在上游开流前先执行 prepare，用于模拟 handler 在
// 开流前已经写给客户端的字节（例如等槽 ping）。
func runPreOutputStageStreamWith(t *testing.T, path string, gateway config.GatewayConfig, body io.ReadCloser, prepare func(*gin.Context)) preOutputStageRun {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if gateway.MaxLineSize == 0 {
		gateway.MaxLineSize = defaultMaxLineSize
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: gateway}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if prepare != nil {
		prepare(c)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "acc"}
	var err error
	if path == "native" {
		_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	} else {
		_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	}
	return preOutputStageRun{err: err, body: rec.Body.String(), c: c}
}

func requirePreOutputFailover(t *testing.T, run preOutputStageRun) {
	t.Helper()
	require.Error(t, run.err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, run.err, &failoverErr)
	require.True(t, failoverErr.RequestScopedTransient)
	require.False(t, run.c.Writer.Written())
	require.Empty(t, run.body)
}

func requirePreOutputDelivered(t *testing.T, run preOutputStageRun) {
	t.Helper()
	require.Error(t, run.err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, run.err, &failoverErr)
	require.Contains(t, run.body, "response.failed")
}

// 线上形态（#295）：created / in_progress 之后上游先推一个不可见的结构事件，再以
// seq=3 error、seq=4 response.failed 降载收尾。开关关闭时维持现状（过载交给客户端）；
// 开关打开后暂存区不提交，交给原有 failover。
func TestOpenAIPreOutputStagingInvisibleStructuralEventBeforeOverload(t *testing.T) {
	seq2Events := map[string]string{
		"encrypted_reasoning": preOutputStageReasoning,
		"unknown_event":       preOutputStageUnknown,
	}
	for name, seq2 := range seq2Events {
		stream := preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, seq2, preOutputStageOverload, preOutputStageFailed)
		for _, path := range []string{"native", "passthrough"} {
			t.Run(name+"/"+path+"/disabled", func(t *testing.T) {
				useOpenAIPreOutputStaging(t, defaultOpenAIPreOutputStagingSettings())
				requirePreOutputDelivered(t, runPreOutputStageStream(t, path, config.GatewayConfig{}, io.NopCloser(strings.NewReader(stream))))
			})
			t.Run(name+"/"+path+"/enabled", func(t *testing.T) {
				useOpenAIPreOutputStaging(t, enabledPreOutputStaging())
				requirePreOutputFailover(t, runPreOutputStageStream(t, path, config.GatewayConfig{}, io.NopCloser(strings.NewReader(stream))))
			})
		}
	}
}

// immediate lifecycle 开启时 created 一到就提交，暂存开关不起作用。
func TestOpenAIPreOutputStagingYieldsToImmediateLifecycle(t *testing.T) {
	stream := preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageReasoning, preOutputStageOverload, preOutputStageFailed)
	for _, path := range []string{"native", "passthrough"} {
		t.Run(path, func(t *testing.T) {
			gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{
				openAITTFTMode:                 OpenAITTFTModeSemantic,
				openAIImmediateLifecycleEvents: true,
				openAIPreOutputStaging:         enabledPreOutputStaging(),
				expiresAt:                      time.Now().Add(time.Minute).UnixNano(),
			})
			t.Cleanup(func() {
				gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: OpenAITTFTModeSemantic, expiresAt: time.Now().Add(time.Minute).UnixNano()})
			})
			run := runPreOutputStageStream(t, path, config.GatewayConfig{}, io.NopCloser(strings.NewReader(stream)))
			requirePreOutputDelivered(t, run)
			require.Contains(t, run.body, "response.created")
		})
	}
}

// 正常流在开关打开时输出与关闭时逐字节一致。
func TestOpenAIPreOutputStagingPreservesSuccessfulStream(t *testing.T) {
	stream := preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageReasoning, preOutputStageDelta, preOutputStageCompleted)
	for _, path := range []string{"native", "passthrough"} {
		t.Run(path, func(t *testing.T) {
			useOpenAIPreOutputStaging(t, defaultOpenAIPreOutputStagingSettings())
			disabled := runPreOutputStageStream(t, path, config.GatewayConfig{}, io.NopCloser(strings.NewReader(stream)))
			require.NoError(t, disabled.err)

			useOpenAIPreOutputStaging(t, enabledPreOutputStaging())
			enabled := runPreOutputStageStream(t, path, config.GatewayConfig{}, io.NopCloser(strings.NewReader(stream)))
			require.NoError(t, enabled.err)
			require.Equal(t, disabled.body, enabled.body)
			require.Contains(t, enabled.body, "rs_1")
			require.Contains(t, enabled.body, "response.completed")
		})
	}
}

// 上限触发即提交暂存区并按原逻辑继续转发：之后的过载不再换号，客户端收到暂存的事件。
func TestOpenAIPreOutputStagingLimitsCommitInsteadOfFailover(t *testing.T) {
	bigReasoning := `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_big","type":"reasoning","encrypted_content":"` +
		strings.Repeat("x", 80<<10) + `","summary":[]}}`
	cases := map[string]struct {
		cfg    OpenAIPreOutputStagingSettings
		stream string
		marker string
	}{
		"max_staged_events": {
			cfg:    OpenAIPreOutputStagingSettings{Enabled: true, MaxStagedEvents: 2, StageMaxBytes: defaultOpenAIPreOutputStageMaxBytes},
			stream: preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageReasoning, preOutputStageOverload, preOutputStageFailed),
			marker: "rs_1",
		},
		"max_stage_bytes": {
			cfg:    OpenAIPreOutputStagingSettings{Enabled: true, MaxStagedEvents: defaultOpenAIPreOutputMaxStagedEvents, StageMaxBytes: minOpenAIPreOutputStageMaxBytes},
			stream: preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, bigReasoning, preOutputStageOverload, preOutputStageFailed),
			marker: "rs_big",
		},
	}
	for name, tc := range cases {
		for _, path := range []string{"native", "passthrough"} {
			t.Run(name+"/"+path, func(t *testing.T) {
				useOpenAIPreOutputStaging(t, tc.cfg)
				run := runPreOutputStageStream(t, path, config.GatewayConfig{}, io.NopCloser(strings.NewReader(tc.stream)))
				requirePreOutputDelivered(t, run)
				require.Contains(t, run.body, "response.created")
				require.Contains(t, run.body, tc.marker)
			})
		}
	}
}

// 暂存时长上限：原生路径在上游静默时由定时器提交，此后的过载不再换号；
// 透传路径是同步读循环，只在下一个非失败事件到达时检查，静默后直接过载仍可换号。
func TestOpenAIPreOutputStagingMaxStageMsCommits(t *testing.T) {
	cfg := enabledPreOutputStaging()
	cfg.MaxStageMs = 50
	for _, tc := range []struct {
		path      string
		committed bool
	}{{"native", true}, {"passthrough", false}} {
		t.Run(tc.path+"/silent", func(t *testing.T) {
			useOpenAIPreOutputStaging(t, cfg)
			pr, pw := io.Pipe()
			go func() {
				defer func() { _ = pw.Close() }()
				_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress))
				time.Sleep(200 * time.Millisecond)
				_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageOverload, preOutputStageFailed))
			}()
			run := runPreOutputStageStream(t, tc.path, config.GatewayConfig{}, pr)
			if tc.committed {
				requirePreOutputDelivered(t, run)
				require.Contains(t, run.body, "response.created")
				return
			}
			requirePreOutputFailover(t, run)
		})
		t.Run(tc.path+"/next_event", func(t *testing.T) {
			useOpenAIPreOutputStaging(t, cfg)
			pr, pw := io.Pipe()
			go func() {
				defer func() { _ = pw.Close() }()
				_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress))
				time.Sleep(200 * time.Millisecond)
				_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageReasoning, preOutputStageOverload, preOutputStageFailed))
			}()
			run := runPreOutputStageStream(t, tc.path, config.GatewayConfig{}, pr)
			requirePreOutputDelivered(t, run)
			require.Contains(t, run.body, "rs_1")
		})
	}
}

// preOutputStageTimedRecorder 记录首个可见文字事件真正写到客户端的时刻。
type preOutputStageTimedRecorder struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	start time.Time
	first time.Duration
}

func (r *preOutputStageTimedRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	if r.first == 0 && strings.Contains(string(b), "response.output_text.delta") {
		r.first = time.Since(r.start)
	}
	r.mu.Unlock()
	return r.ResponseRecorder.Write(b)
}

func (r *preOutputStageTimedRecorder) firstVisible() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.first
}

// #319：首个可见输出在时长/事件数上限之前到达时必须立即写出。上游每个 SSE 事件一次
// 到达，处理 data 行时同一事件的结尾空行已在队列里；semantic 口径下推理条目又已记过首字，
// 修复前可见文字会被压在暂存区直到流结束。
func TestOpenAIPreOutputStagingVisibleCommitFlushesBeforeLimits(t *testing.T) {
	cfg := enabledPreOutputStaging()
	cfg.MaxStageMs = 1000
	cfg.MaxStagedEvents = 6
	for _, path := range []string{"native", "passthrough"} {
		for _, mode := range []string{OpenAITTFTModeSemantic, OpenAITTFTModeVisible} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				useOpenAIPreOutputStaging(t, cfg)
				gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{
					openAITTFTMode:         mode,
					openAIPreOutputStaging: cfg,
					expiresAt:              time.Now().Add(time.Minute).UnixNano(),
				})
				gin.SetMode(gin.TestMode)
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize, StreamKeepaliveInterval: 1}}}
				rec := &preOutputStageTimedRecorder{ResponseRecorder: httptest.NewRecorder(), start: time.Now()}
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				pr, pw := io.Pipe()
				go func() {
					defer func() { _ = pw.Close() }()
					_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageReasoning))
					time.Sleep(300 * time.Millisecond)
					for i := 0; i < 30; i++ {
						_, _ = io.WriteString(pw, preOutputStageSSE(fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,"output_index":1,"content_index":0,"delta":"d%d"}`, i+3, i)))
						time.Sleep(50 * time.Millisecond)
					}
					_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageCompleted))
				}()
				resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "acc"}
				var err error
				if path == "native" {
					_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				} else {
					_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				}
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), "response.completed")
				// 上游 300ms 发首个文字、约 1.8s 结束（晚于时长上限）；被压到流结束时首字会超过上限。
				first := rec.firstVisible()
				require.NotZero(t, first)
				require.Less(t, first, cfg.stageDuration())
			})
		}
	}
}

// #321：时长上限在不可见事件写到一半时到期不能作废，事件完成后补一次提交；
// 之后上游静默再过载时交给客户端而不是换号。
func TestOpenAIPreOutputStagingMaxStageMsDuringEventCommitsAfterEvent(t *testing.T) {
	cfg := enabledPreOutputStaging()
	cfg.MaxStageMs = 50
	useOpenAIPreOutputStaging(t, cfg)
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress))
		// 推理条目只到 data 行，结尾空行在时长上限之后才到。
		_, _ = io.WriteString(pw, strings.TrimSuffix(preOutputStageSSE(preOutputStageReasoning), "\n"))
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(pw, "\n")
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageOverload, preOutputStageFailed))
	}()
	run := runPreOutputStageStream(t, "native", config.GatewayConfig{}, pr)
	requirePreOutputDelivered(t, run)
	require.Contains(t, run.body, "rs_1")
}

// 结构事件之后要提交的行放不进暂存区（含超过 8 MiB 暂存硬限）时不再暂缓：先提交暂存区，
// 当前行直接写给客户端，不能被当成换号失败。分别覆盖同步扫描（max_stage_ms=0）与异步循环。
func TestOpenAIPreOutputStagingOversizedLineCommitsInsteadOfFailover(t *testing.T) {
	const size = openAIFirstOutputStageMaxBytes + 1<<20
	imageAdded := `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"ig_1","type":"image_generation_call","status":"in_progress"}}`
	imageDone := `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"` +
		strings.Repeat("A", size) + `"}}`
	hugeReasoning := `{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"id":"rs_huge","type":"reasoning","encrypted_content":"` +
		strings.Repeat("x", size) + `","summary":[]}}`
	completed := `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`
	streams := map[string]struct {
		stream string
		marker string
		failed bool
	}{
		"visible_image_result": {preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, imageAdded, imageDone, completed), "ig_1", false},
		"invisible_reasoning":  {preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageReasoning, hugeReasoning, preOutputStageOverload, preOutputStageFailed), "rs_huge", true},
	}
	for name, tc := range streams {
		for _, maxStageMs := range []int{0, defaultOpenAIPreOutputMaxStageMs} {
			for _, path := range []string{"native", "passthrough"} {
				t.Run(name+"/"+path+"/max_stage_ms="+strconv.Itoa(maxStageMs), func(t *testing.T) {
					cfg := enabledPreOutputStaging()
					cfg.MaxStageMs = maxStageMs
					useOpenAIPreOutputStaging(t, cfg)
					run := runPreOutputStageStream(t, path, config.GatewayConfig{}, io.NopCloser(strings.NewReader(tc.stream)))
					if tc.failed {
						requirePreOutputDelivered(t, run)
					} else {
						require.NoError(t, run.err)
						require.Contains(t, run.body, "response.completed")
					}
					createdAt := strings.Index(run.body, "response.created")
					markerAt := strings.Index(run.body, tc.marker)
					require.GreaterOrEqual(t, createdAt, 0)
					require.Greater(t, markerAt, createdAt)
					require.Greater(t, len(run.body), size)
				})
			}
		}
	}
}

// 只有 encrypted 推理的长推理阶段仍算结构性进度，不触发首输出超时。
func TestOpenAIPreOutputStagingKeepsStructuralProgressForFirstOutputTimeout(t *testing.T) {
	cfg := enabledPreOutputStaging()
	cfg.MaxStageMs = 0
	useOpenAIPreOutputStaging(t, cfg)
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageCreated, preOutputStageInProgress, preOutputStageReasoning))
		time.Sleep(1300 * time.Millisecond)
		_, _ = io.WriteString(pw, preOutputStageSSE(preOutputStageDelta, preOutputStageCompleted))
	}()
	run := runPreOutputStageStream(t, "native", config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}, pr)
	require.NoError(t, run.err)
	require.Contains(t, run.body, "rs_1")
	require.Contains(t, run.body, "response.completed")
}

func TestOpenAIStreamCommitsClientOutput(t *testing.T) {
	cases := []struct {
		data      string
		eventType string
		immediate bool
		want      bool
	}{
		{preOutputStageCreated, "response.created", false, false},
		{preOutputStageCreated, "response.created", true, true},
		{preOutputStageReasoning, "response.output_item.added", false, false},
		{preOutputStageUnknown, "response.unrecognized_progress", false, false},
		{`{"type":"response.output_item.added","item":{"type":"function_call","name":"f","arguments":""}}`, "response.output_item.added", false, false},
		{`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, "response.reasoning_summary_text.delta", false, true},
		{preOutputStageDelta, "response.output_text.delta", false, true},
		{`{"type":"response.function_call_arguments.delta","delta":"{\"a\""}`, "response.function_call_arguments.delta", false, true},
		{preOutputStageCompleted, "response.completed", false, true},
		{`{"type":"response.incomplete","response":{"id":"resp_1"}}`, "response.incomplete", false, true},
		{`[DONE]`, "", false, true},
		{preOutputStageOverload, "error", false, false},
		{`{"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"blocked"}}`, "error", false, true},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, openAIStreamCommitsClientOutput(tc.data, tc.eventType, tc.immediate), "data=%s", tc.data)
	}
}

func TestParseOpenAIPreOutputStagingSettings(t *testing.T) {
	require.Equal(t, defaultOpenAIPreOutputStagingSettings(), parseOpenAIPreOutputStagingSettings(map[string]string{}))

	cfg := parseOpenAIPreOutputStagingSettings(map[string]string{
		SettingKeyOpenAIPreOutputStagingEnabled:  "true",
		SettingKeyOpenAIPreOutputMaxStagedEvents: "4",
		SettingKeyOpenAIPreOutputStageMaxBytes:   strconv.Itoa(128 << 10),
		SettingKeyOpenAIPreOutputMaxStageMs:      "0",
	})
	require.Equal(t, OpenAIPreOutputStagingSettings{Enabled: true, MaxStagedEvents: 4, StageMaxBytes: 128 << 10, MaxStageMs: 0}, cfg)

	// 越界或非法值回退默认，保证热路径配置可用。
	cfg = parseOpenAIPreOutputStagingSettings(map[string]string{
		SettingKeyOpenAIPreOutputMaxStagedEvents: "0",
		SettingKeyOpenAIPreOutputStageMaxBytes:   strconv.Itoa(16 << 20),
		SettingKeyOpenAIPreOutputMaxStageMs:      "abc",
	})
	require.Equal(t, defaultOpenAIPreOutputStagingSettings(), cfg)
}

func TestValidateOpenAIPreOutputStagingSettings(t *testing.T) {
	require.NoError(t, validateOpenAIPreOutputStagingSettings(defaultOpenAIPreOutputStagingSettings()))
	for _, cfg := range []OpenAIPreOutputStagingSettings{
		{MaxStagedEvents: 0, StageMaxBytes: defaultOpenAIPreOutputStageMaxBytes},
		{MaxStagedEvents: 257, StageMaxBytes: defaultOpenAIPreOutputStageMaxBytes},
		{MaxStagedEvents: 16, StageMaxBytes: 1024},
		{MaxStagedEvents: 16, StageMaxBytes: 9 << 20},
		{MaxStagedEvents: 16, StageMaxBytes: defaultOpenAIPreOutputStageMaxBytes, MaxStageMs: -1},
		{MaxStagedEvents: 16, StageMaxBytes: defaultOpenAIPreOutputStageMaxBytes, MaxStageMs: 600001},
	} {
		require.Error(t, validateOpenAIPreOutputStagingSettings(cfg), "%+v", cfg)
	}
}
