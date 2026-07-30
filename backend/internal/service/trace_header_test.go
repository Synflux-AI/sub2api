//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
	"github.com/gin-gonic/gin"
)

// traceAccount 构造一个仅设置 trace_id_passthrough 的账号；enabled 为 nil 时不写该字段。
func traceAccount(enabled any) *Account {
	if enabled == nil {
		return &Account{}
	}
	return &Account{Extra: map[string]any{"trace_id_passthrough": enabled}}
}

func TestInjectTraceHeader(t *testing.T) {
	const traceValue = "trace-abc-123"
	overlong := strings.Repeat("a", traceid.MaxBytes+1)
	maxLen := strings.Repeat("b", traceid.MaxBytes)

	tests := []struct {
		name string
		// account 为出站账号
		account *Account
		// ctxValue 为 ctxkey.TraceID 在 context 中的值；nil 表示不写入该 key
		ctxValue any
		// clientHeader 模拟客户端自带的 X-Trace-Id（白名单拷贝后残留的值）
		clientHeader string
		want         string
	}{
		{
			name:     "开关缺省关闭时不注入",
			account:  traceAccount(nil),
			ctxValue: traceValue,
			want:     "",
		},
		{
			name:     "开关显式关闭时不注入",
			account:  traceAccount(false),
			ctxValue: traceValue,
			want:     "",
		},
		{
			name:     "开关开启且 ctx 有合法 trace 时注入原值",
			account:  traceAccount(true),
			ctxValue: traceValue,
			want:     traceValue,
		},
		{
			name:     "开关开启但 ctx 无 trace 时不注入",
			account:  traceAccount(true),
			ctxValue: nil,
			want:     "",
		},
		{
			name:     "开关开启但 ctx 中 trace 为空串时不注入",
			account:  traceAccount(true),
			ctxValue: "   ",
			want:     "",
		},
		{
			name:     "开关开启但 ctx 中 trace 超长时被拒绝",
			account:  traceAccount(true),
			ctxValue: overlong,
			want:     "",
		},
		{
			name:     "开关开启且 trace 恰好等于长度上限时注入",
			account:  traceAccount(true),
			ctxValue: maxLen,
			want:     maxLen,
		},
		{
			name:     "开关开启但 ctx 值非字符串时不注入",
			account:  traceAccount(true),
			ctxValue: 42,
			want:     "",
		},
		{
			name:         "开关开启时客户端自带的同名头被服务端值覆盖",
			account:      traceAccount(true),
			ctxValue:     traceValue,
			clientHeader: "client-forged-id",
			want:         traceValue,
		},
		{
			name:         "开关关闭时客户端自带的同名头保持原样（helper 不做清理）",
			account:      traceAccount(false),
			ctxValue:     traceValue,
			clientHeader: "client-forged-id",
			want:         "client-forged-id",
		},
		{
			name:     "账号为 nil 时不注入",
			account:  nil,
			ctxValue: traceValue,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(t)
			if tt.clientHeader != "" {
				req.Header.Set(traceid.Header, tt.clientHeader)
			}
			ctx := context.Background()
			if tt.ctxValue != nil {
				ctx = context.WithValue(ctx, ctxkey.TraceID, tt.ctxValue)
			}

			injectTraceHeader(ctx, req, tt.account)

			if got := req.Header.Get(traceid.Header); got != tt.want {
				t.Fatalf("X-Trace-Id = %q, want %q", got, tt.want)
			}
			// 不得因注入破坏其他头
			if got := req.Header.Get("Authorization"); got != "Bearer original-token" {
				t.Fatalf("Authorization 被意外修改: %q", got)
			}
		})
	}
}

func TestInjectTraceHeaderNilInputs(t *testing.T) {
	account := traceAccount(true)
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-abc-123")

	// req 为 nil 不得 panic
	injectTraceHeader(ctx, nil, account)

	// ctx 为 nil 不得 panic，且不注入
	req := newRequest(t)
	injectTraceHeader(nil, req, account) //nolint:staticcheck // 显式验证 nil ctx 的防御分支
	if got := req.Header.Get(traceid.Header); got != "" {
		t.Fatalf("ctx 为 nil 时不应注入，got %q", got)
	}
}

// TestBuildCountTokensRequest_InjectsTraceHeader 锁定「落点确实出头」这一不变量：
// 上面的用例只验证 helper 本身，若有人把某个落点的调用删掉或移到白名单拷贝之前，
// 那些用例仍会全绿。这里选 buildCountTokensRequest 作为代表落点（构造成本最低：
// 已有邻居测试在用同一套 gin.CreateTestContext + &GatewayService{cfg} 组合），
// 断言最终出站 req 上 X-Trace-Id 等于 ctx 中的值。
func TestBuildCountTokensRequest_InjectsTraceHeader(t *testing.T) {
	const traceValue = "trace-site-level-1"

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	// 客户端自带值：x-trace-id 不在任何白名单，故它到不了出站 req；
	// 这里只是确保「客户端能污染服务端值」这条路今天确实不存在。
	c.Request.Header.Set(traceid.Header, "client-supplied")

	account := &Account{
		ID: 4213, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-ant-xxx"},
		Status:      StatusActive, Schedulable: true,
		Extra: map[string]any{"trace_id_passthrough": true},
	}
	body := []byte(`{"model":"claude-haiku-4-5","messages":[]}`)
	ctx := context.WithValue(context.Background(), ctxkey.TraceID, traceValue)

	svc := &GatewayService{cfg: &config.Config{}}
	req, _, err := svc.buildCountTokensRequest(
		ctx, c, account, body,
		"sk-ant-xxx", "apikey", "claude-haiku-4-5", false,
	)
	if err != nil {
		t.Fatalf("buildCountTokensRequest 失败: %v", err)
	}

	if got := req.Header.Get(traceid.Header); got != traceValue {
		t.Fatalf("落点未注入 X-Trace-Id: got %q, want %q", got, traceValue)
	}
	// 只应有一份，避免将来白名单/注入两条路同时写入
	if values := req.Header.Values(traceid.Header); len(values) != 1 {
		t.Fatalf("X-Trace-Id 应恰好一份，got %v", values)
	}

	// 开关关闭时同一落点不得出头
	account.Extra = nil
	reqOff, _, err := svc.buildCountTokensRequest(
		ctx, c, account, body,
		"sk-ant-xxx", "apikey", "claude-haiku-4-5", false,
	)
	if err != nil {
		t.Fatalf("buildCountTokensRequest（开关关闭）失败: %v", err)
	}
	if got := reqOff.Header.Get(traceid.Header); got != "" {
		t.Fatalf("开关关闭时落点不应注入，got %q", got)
	}
}
