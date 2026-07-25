//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
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
