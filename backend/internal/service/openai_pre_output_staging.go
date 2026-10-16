package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// OpenAI pre-output 暂存（#295）。
//
// OpenAI 流式路径在首个输出前暂存上游事件，被判定为"客户端输出开始"后才提交；
// 提交之后的流内失败不再换号。原有判定（openAIStreamStartsClientOutput）把
// 只带 encrypted_content 的推理条目、未识别事件等不可见结构事件也算作输出，
// 上游降载若排在这类事件之后（created → in_progress → 结构事件 → error →
// response.failed），过载就会原样交给客户端。
//
// 开关打开后把"结构性进度"与"提交"拆开：结构事件仍解除首输出超时，但只有可见
// 内容、终止事件才提交；事件数、字节数、暂存时长三个上限任一触发即提交并按原
// 逻辑继续转发（绝不因此换号），最坏情况退回开关关闭时的行为。暂存期间失败是否
// 换号仍由原有 failover / 错误处理规则判定。
const (
	defaultOpenAIPreOutputMaxStagedEvents = 16
	minOpenAIPreOutputMaxStagedEvents     = 1
	maxOpenAIPreOutputMaxStagedEvents     = 256

	defaultOpenAIPreOutputStageMaxBytes = 1 << 20
	minOpenAIPreOutputStageMaxBytes     = 64 << 10
	maxOpenAIPreOutputStageMaxBytes     = openAIFirstOutputStageMaxBytes

	defaultOpenAIPreOutputMaxStageMs = 5000
	maxOpenAIPreOutputMaxStageMs     = 600000
)

// 暂存内容被提交给客户端的原因，用于诊断日志。
const (
	openAIPreOutputCommitVisible   = "visible_output"
	openAIPreOutputCommitMaxEvents = "max_staged_events"
	openAIPreOutputCommitMaxBytes  = "max_stage_bytes"
	openAIPreOutputCommitMaxTime   = "max_stage_ms"
)

// OpenAIPreOutputStagingSettings 是 openai_pre_output_* 系统设置的运行时视图。
type OpenAIPreOutputStagingSettings struct {
	Enabled         bool
	MaxStagedEvents int
	StageMaxBytes   int
	MaxStageMs      int
}

func defaultOpenAIPreOutputStagingSettings() OpenAIPreOutputStagingSettings {
	return OpenAIPreOutputStagingSettings{
		MaxStagedEvents: defaultOpenAIPreOutputMaxStagedEvents,
		StageMaxBytes:   defaultOpenAIPreOutputStageMaxBytes,
		MaxStageMs:      defaultOpenAIPreOutputMaxStageMs,
	}
}

// parseOpenAIPreOutputStagingSettings 读取已存储的值；缺失或越界的数值回退默认值，
// 保证热路径拿到的配置总是可用的。
func parseOpenAIPreOutputStagingSettings(values map[string]string) OpenAIPreOutputStagingSettings {
	cfg := defaultOpenAIPreOutputStagingSettings()
	cfg.Enabled = values[SettingKeyOpenAIPreOutputStagingEnabled] == "true"
	cfg.MaxStagedEvents = parseBoundedSettingInt(values[SettingKeyOpenAIPreOutputMaxStagedEvents], cfg.MaxStagedEvents, minOpenAIPreOutputMaxStagedEvents, maxOpenAIPreOutputMaxStagedEvents)
	cfg.StageMaxBytes = parseBoundedSettingInt(values[SettingKeyOpenAIPreOutputStageMaxBytes], cfg.StageMaxBytes, minOpenAIPreOutputStageMaxBytes, maxOpenAIPreOutputStageMaxBytes)
	cfg.MaxStageMs = parseBoundedSettingInt(values[SettingKeyOpenAIPreOutputMaxStageMs], cfg.MaxStageMs, 0, maxOpenAIPreOutputMaxStageMs)
	return cfg
}

func parseBoundedSettingInt(raw string, fallback, minValue, maxValue int) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < minValue || v > maxValue {
		return fallback
	}
	return v
}

func validateOpenAIPreOutputStagingSettings(cfg OpenAIPreOutputStagingSettings) error {
	if cfg.MaxStagedEvents < minOpenAIPreOutputMaxStagedEvents || cfg.MaxStagedEvents > maxOpenAIPreOutputMaxStagedEvents {
		return fmt.Errorf("%s must be between %d and %d", SettingKeyOpenAIPreOutputMaxStagedEvents, minOpenAIPreOutputMaxStagedEvents, maxOpenAIPreOutputMaxStagedEvents)
	}
	if cfg.StageMaxBytes < minOpenAIPreOutputStageMaxBytes || cfg.StageMaxBytes > maxOpenAIPreOutputStageMaxBytes {
		return fmt.Errorf("%s must be between %d and %d", SettingKeyOpenAIPreOutputStageMaxBytes, minOpenAIPreOutputStageMaxBytes, maxOpenAIPreOutputStageMaxBytes)
	}
	if cfg.MaxStageMs < 0 || cfg.MaxStageMs > maxOpenAIPreOutputMaxStageMs {
		return fmt.Errorf("%s must be between 0 and %d", SettingKeyOpenAIPreOutputMaxStageMs, maxOpenAIPreOutputMaxStageMs)
	}
	return nil
}

func (cfg OpenAIPreOutputStagingSettings) stageDuration() time.Duration {
	return time.Duration(cfg.MaxStageMs) * time.Millisecond
}

// openAIPreOutputStaging 每个请求读一次，避免同一条流中途切换行为。
// immediate lifecycle 开启时 created 一到就提交，暂存无从谈起，按关闭处理。
func (s *OpenAIGatewayService) openAIPreOutputStaging(ctx context.Context, immediateLifecycle bool) OpenAIPreOutputStagingSettings {
	cfg := defaultOpenAIPreOutputStagingSettings()
	if s != nil && s.settingService != nil {
		cfg = s.settingService.GetOpenAIPreOutputStagingSettings(ctx)
	} else if cached, ok := gatewayForwardingCache.Load().(*cachedGatewayForwardingSettings); ok && cached != nil &&
		(cached.expiresAt == 0 || time.Now().UnixNano() < cached.expiresAt) {
		cfg = cached.openAIPreOutputStaging
	}
	if immediateLifecycle {
		cfg.Enabled = false
	}
	return cfg
}

// openAIStreamCommitsClientOutput 决定暂存开关打开时是否把 pre-output 暂存提交给客户端。
// 它只收窄 openAIStreamStartsClientOutput：后者仍代表结构性进度（解除首输出超时），
// 这里额外要求事件对客户端可见或是终止事件。
func openAIStreamCommitsClientOutput(data, eventType string, immediateLifecycle bool) bool {
	if !openAIStreamStartsClientOutput(data, eventType, immediateLifecycle) {
		return false
	}
	trimmed := strings.TrimSpace(data)
	if trimmed == "[DONE]" {
		return true
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" && gjson.Valid(trimmed) {
		eventType = strings.TrimSpace(gjson.Get(trimmed, "type").String())
	}
	switch eventType {
	case "response.created", "response.in_progress", "error":
		// 走到这里说明 immediate lifecycle 已开启，或 error 帧不可重试：维持原语义。
		return true
	}
	if openAIStreamEventTypeIsTerminal(eventType) {
		return true
	}
	return openAIStreamDataStartsVisibleOutput(trimmed, eventType)
}

// describeOpenAIStreamCommitEvent 生成诊断用的事件描述（type 加 item.type），不含任何内容字段。
func describeOpenAIStreamCommitEvent(data, eventType string) string {
	desc := strings.TrimSpace(eventType)
	trimmed := strings.TrimSpace(data)
	if desc == "" && gjson.Valid(trimmed) {
		desc = strings.TrimSpace(gjson.Get(trimmed, "type").String())
	}
	if desc == "" {
		desc = "unknown"
	}
	if itemType := strings.TrimSpace(gjson.Get(trimmed, "item.type").String()); itemType != "" {
		desc += "/" + itemType
	}
	return desc
}

func (s *SystemSettings) openAIPreOutputStagingSettings() OpenAIPreOutputStagingSettings {
	return OpenAIPreOutputStagingSettings{
		Enabled:         s.OpenAIPreOutputStagingEnabled,
		MaxStagedEvents: s.OpenAIPreOutputMaxStagedEvents,
		StageMaxBytes:   s.OpenAIPreOutputStageMaxBytes,
		MaxStageMs:      s.OpenAIPreOutputMaxStageMs,
	}
}

// ValidateOpenAIPreOutputStagingSystemSettings 供管理台在保存前校验，越界时返回可直接展示的错误。
func ValidateOpenAIPreOutputStagingSystemSettings(s *SystemSettings) error {
	if s == nil {
		return nil
	}
	return validateOpenAIPreOutputStagingSettings(s.openAIPreOutputStagingSettings())
}

// openAIPreOutputStagingLimitReason 判断再暂存当前事件是否触及任一上限；触及时当前事件随暂存区一并提交。
// stagedEvents 含当前事件，stagedBytes 为写入当前行后的暂存字节数。
func openAIPreOutputStagingLimitReason(cfg OpenAIPreOutputStagingSettings, stagedEvents int, stagedBytes int64, elapsed time.Duration) string {
	switch {
	case cfg.MaxStagedEvents > 0 && stagedEvents > cfg.MaxStagedEvents:
		return openAIPreOutputCommitMaxEvents
	case cfg.StageMaxBytes > 0 && stagedBytes > int64(cfg.StageMaxBytes):
		return openAIPreOutputCommitMaxBytes
	case cfg.MaxStageMs > 0 && elapsed >= cfg.stageDuration():
		return openAIPreOutputCommitMaxTime
	}
	return ""
}
