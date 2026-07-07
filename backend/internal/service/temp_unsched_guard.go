package service

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// tempUnschedDisabledResolver 由 wire 在进程启动时注入
// （SettingService.IsTempUnschedDisabled），供无法直接持有 SettingService 的
// 调用点（token provider / token refresh / 错误率监控 / 包级函数等）查询
// "禁止临时停止调度"全局开关。未注入时视为 false（不禁止，保持原有行为）。
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

// tempUnschedDisabledSkip 开关开启时返回 true 并记录被跳过的临时停用动作，
// 供各触发点在执行任何副作用（内存阻断 / DB / Redis）之前统一检查。
func tempUnschedDisabledSkip(ctx context.Context, accountID int64, source string) bool {
	if !isTempUnschedDisabled(ctx) {
		return false
	}
	slog.Info("temp_unsched_skipped_by_global_switch", "account_id", accountID, "source", source)
	return true
}
