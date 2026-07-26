package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccount_IsTraceIDPassthroughEnabled(t *testing.T) {
	t.Run("显式开启", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"trace_id_passthrough": true,
			},
		}
		require.True(t, account.IsTraceIDPassthroughEnabled())
	})

	t.Run("显式关闭", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"trace_id_passthrough": false,
			},
		}
		require.False(t, account.IsTraceIDPassthroughEnabled())
	})

	t.Run("字段类型非法默认关闭", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"trace_id_passthrough": "true",
			},
		}
		require.False(t, account.IsTraceIDPassthroughEnabled())
	})

	t.Run("Extra 为 nil 默认关闭", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
		}
		require.False(t, account.IsTraceIDPassthroughEnabled())
	})

	t.Run("receiver 为 nil 默认关闭", func(t *testing.T) {
		var account *Account
		require.False(t, account.IsTraceIDPassthroughEnabled())
	})

	t.Run("不按 platform / type 收窄，OpenAI 账号同样生效", func(t *testing.T) {
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"trace_id_passthrough": true,
			},
		}
		require.True(t, account.IsTraceIDPassthroughEnabled())
	})
}
