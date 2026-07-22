package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// newRectifierGatewayService 构造一个仅依赖 settingService 的 GatewayService，
// 并把指定的 RectifierSettings 写入内存仓库，供签名整流/切换相关方法测试使用。
func newRectifierGatewayService(t *testing.T, settings *RectifierSettings) *GatewayService {
	t.Helper()
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := &GatewayService{
		settingService: NewSettingService(repo, &config.Config{}),
	}
	require.NoError(t, svc.settingService.SetRectifierSettings(context.Background(), settings))
	return svc
}

// signatureErrorBody 复刻上游真实返回的签名错误响应体（含 "signature" 关键词）。
var signatureErrorBody = []byte(`{"error":{"message":"messages.3.content.0: invalid (empty) signature in thinking block","type":"invalid_request_error"}}`)

// nonSignatureErrorBody 一个与签名无关的 400 错误体。
var nonSignatureErrorBody = []byte(`{"error":{"message":"max_tokens: must be greater than 0","type":"invalid_request_error"}}`)

// deserializeErrorBody 复刻中转上游对请求体反序列化失败时返回的 422 错误体。
var deserializeErrorBody = []byte(`{"error":{"message":"Failed to deserialize request body at ` + "`messages[29].content`" + `: data did not match any variant of untagged enum MessageContent (request id: 202607221023229216699078268d9d6Kxd185Ke)","type":"upstream_error"},"type":"error"}`)

const strictModel = "claude-sonnet-4-5"

func TestShouldFailoverSignatureError(t *testing.T) {
	apiKeyAccount := &Account{Type: AccountTypeAPIKey, Platform: PlatformAnthropic}
	oauthAccount := &Account{Type: AccountTypeOAuth, Platform: PlatformAnthropic}

	tests := []struct {
		name     string
		account  *Account
		settings *RectifierSettings
		body     []byte
		model    string
		want     bool
	}{
		{
			name:     "api key + enabled + builtin signature error -> failover",
			account:  apiKeyAccount,
			settings: &RectifierSettings{Enabled: true, APIKeySignatureFailoverEnabled: true},
			body:     signatureErrorBody,
			model:    strictModel,
			want:     true,
		},
		{
			name:     "failover switch off -> no failover",
			account:  apiKeyAccount,
			settings: &RectifierSettings{Enabled: true, APIKeySignatureFailoverEnabled: false},
			body:     signatureErrorBody,
			model:    strictModel,
			want:     false,
		},
		{
			name:     "master switch off -> no failover",
			account:  apiKeyAccount,
			settings: &RectifierSettings{Enabled: false, APIKeySignatureFailoverEnabled: true},
			body:     signatureErrorBody,
			model:    strictModel,
			want:     false,
		},
		{
			name:     "oauth account -> out of scope, no failover",
			account:  oauthAccount,
			settings: &RectifierSettings{Enabled: true, APIKeySignatureFailoverEnabled: true},
			body:     signatureErrorBody,
			model:    strictModel,
			want:     false,
		},
		{
			name:    "non-signature error matched by custom pattern -> failover",
			account: apiKeyAccount,
			settings: &RectifierSettings{
				Enabled:                        true,
				APIKeySignatureFailoverEnabled: true,
				APIKeySignaturePatterns:        []string{"must be greater than 0"},
			},
			body:  nonSignatureErrorBody,
			model: strictModel,
			want:  true,
		},
		{
			name:     "non-signature error without matching pattern -> no failover",
			account:  apiKeyAccount,
			settings: &RectifierSettings{Enabled: true, APIKeySignatureFailoverEnabled: true},
			body:     nonSignatureErrorBody,
			model:    strictModel,
			want:     false,
		},
		{
			name:     "non-anthropic-strict model -> no failover",
			account:  apiKeyAccount,
			settings: &RectifierSettings{Enabled: true, APIKeySignatureFailoverEnabled: true},
			body:     signatureErrorBody,
			model:    "deepseek-chat",
			want:     false,
		},
		{
			name:    "deserialize error matched by custom pattern -> failover",
			account: apiKeyAccount,
			settings: &RectifierSettings{
				Enabled:                        true,
				APIKeySignatureFailoverEnabled: true,
				APIKeySignaturePatterns:        []string{"Failed to deserialize request body"},
			},
			body:  deserializeErrorBody,
			model: strictModel,
			want:  true,
		},
		{
			name:     "deserialize error without matching pattern -> no failover",
			account:  apiKeyAccount,
			settings: &RectifierSettings{Enabled: true, APIKeySignatureFailoverEnabled: true},
			body:     deserializeErrorBody,
			model:    strictModel,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newRectifierGatewayService(t, tt.settings)
			got := svc.shouldFailoverSignatureError(context.Background(), tt.account, tt.body, tt.model)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestIsSignatureFailoverStatus 固化「签名/关键词切换账号」判定的状态码范围：
// 400（签名错误常规状态码）与 422（中转上游反序列化失败）参与判定，其余不参与。
func TestIsSignatureFailoverStatus(t *testing.T) {
	require.True(t, isSignatureFailoverStatus(400))
	require.True(t, isSignatureFailoverStatus(422))
	for _, status := range []int{401, 403, 404, 409, 413, 429, 500, 529} {
		require.False(t, isSignatureFailoverStatus(status), "status %d", status)
	}
}
