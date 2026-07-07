package service

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// tempUnschedDisabledResolver 由 wire 在进程启动时注入
// （SettingService.IsTempUnschedDisabled），供无法直接持有 SettingService 的
// 调用点（token refresh / 错误率监控等）查询"禁止临时停止调度"全局开关。
// 未注入时视为 false（不禁止，保持原有行为）。
var tempUnschedDisabledResolver atomic.Value // func(context.Context) bool

// SetTempUnschedDisabledResolver 注册"禁止临时停止调度"全局开关解析器（启动时调用一次）。
func SetTempUnschedDisabledResolver(fn func(context.Context) bool) {
	if fn == nil {
		return
	}
	tempUnschedDisabledResolver.Store(fn)
}

// isTempUnschedDisabled 返回系统设置中"禁止临时停止调度"开关是否开启。
func isTempUnschedDisabled(ctx context.Context) bool {
	fn, ok := tempUnschedDisabledResolver.Load().(func(context.Context) bool)
	if !ok || fn == nil {
		return false
	}
	return fn(ctx)
}

// tempUnschedDisabledSkip 当账号属于 Anthropic / OpenAI 平台且开关开启时返回 true，
// 并记录被跳过的临时停用动作。各触发点在执行任何副作用（内存阻断 / DB / Redis）
// 之前统一检查。开关仅对 Anthropic 和 OpenAI 账号生效，其他平台保持原有行为。
func tempUnschedDisabledSkip(ctx context.Context, platform string, accountID int64, source string) bool {
	if platform != PlatformAnthropic && platform != PlatformOpenAI {
		return false
	}
	if !isTempUnschedDisabled(ctx) {
		return false
	}
	slog.Info("temp_unsched_skipped_by_global_switch", "account_id", accountID, "platform", platform, "source", source)
	return true
}
