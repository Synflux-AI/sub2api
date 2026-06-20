package service

// Ops settings models stored in DB `settings` table (JSON blobs).

type OpsEmailNotificationConfig struct {
	Alert  OpsEmailAlertConfig  `json:"alert"`
	Report OpsEmailReportConfig `json:"report"`
}

type OpsEmailAlertConfig struct {
	Enabled               bool     `json:"enabled"`
	Recipients            []string `json:"recipients"`
	MinSeverity           string   `json:"min_severity"`
	RateLimitPerHour      int      `json:"rate_limit_per_hour"`
	BatchingWindowSeconds int      `json:"batching_window_seconds"`
	IncludeResolvedAlerts bool     `json:"include_resolved_alerts"`
}

type OpsEmailReportConfig struct {
	Enabled                         bool     `json:"enabled"`
	Recipients                      []string `json:"recipients"`
	DailySummaryEnabled             bool     `json:"daily_summary_enabled"`
	DailySummarySchedule            string   `json:"daily_summary_schedule"`
	WeeklySummaryEnabled            bool     `json:"weekly_summary_enabled"`
	WeeklySummarySchedule           string   `json:"weekly_summary_schedule"`
	ErrorDigestEnabled              bool     `json:"error_digest_enabled"`
	ErrorDigestSchedule             string   `json:"error_digest_schedule"`
	ErrorDigestMinCount             int      `json:"error_digest_min_count"`
	AccountHealthEnabled            bool     `json:"account_health_enabled"`
	AccountHealthSchedule           string   `json:"account_health_schedule"`
	AccountHealthErrorRateThreshold float64  `json:"account_health_error_rate_threshold"`
}

// OpsEmailNotificationConfigUpdateRequest allows partial updates, while the
// frontend can still send the full config shape.
type OpsEmailNotificationConfigUpdateRequest struct {
	Alert  *OpsEmailAlertConfig  `json:"alert"`
	Report *OpsEmailReportConfig `json:"report"`
}

type OpsDistributedLockSettings struct {
	Enabled    bool   `json:"enabled"`
	Key        string `json:"key"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type OpsAlertSilenceEntry struct {
	RuleID     *int64   `json:"rule_id,omitempty"`
	Severities []string `json:"severities,omitempty"`

	UntilRFC3339 string `json:"until_rfc3339"`
	Reason       string `json:"reason"`
}

type OpsAlertSilencingSettings struct {
	Enabled bool `json:"enabled"`

	GlobalUntilRFC3339 string `json:"global_until_rfc3339"`
	GlobalReason       string `json:"global_reason"`

	Entries []OpsAlertSilenceEntry `json:"entries,omitempty"`
}

type OpsMetricThresholds struct {
	SLAPercentMin               *float64 `json:"sla_percent_min,omitempty"`                 // SLA低于此值变红
	TTFTp99MsMax                *float64 `json:"ttft_p99_ms_max,omitempty"`                 // TTFT P99高于此值变红
	RequestErrorRatePercentMax  *float64 `json:"request_error_rate_percent_max,omitempty"`  // 请求错误率高于此值变红
	UpstreamErrorRatePercentMax *float64 `json:"upstream_error_rate_percent_max,omitempty"` // 上游错误率高于此值变红
}

type OpsRuntimeLogConfig struct {
	Level           string         `json:"level"`
	EnableSampling  bool           `json:"enable_sampling"`
	SamplingInitial int            `json:"sampling_initial"`
	SamplingNext    int            `json:"sampling_thereafter"`
	Caller          bool           `json:"caller"`
	StacktraceLevel string         `json:"stacktrace_level"`
	RetentionDays   int            `json:"retention_days"`
	Source          string         `json:"source,omitempty"`
	UpdatedAt       string         `json:"updated_at,omitempty"`
	UpdatedByUserID int64          `json:"updated_by_user_id,omitempty"`
	Extra           map[string]any `json:"extra,omitempty"`
}

type OpsAlertRuntimeSettings struct {
	EvaluationIntervalSeconds int `json:"evaluation_interval_seconds"`

	DistributedLock OpsDistributedLockSettings `json:"distributed_lock"`
	Silencing       OpsAlertSilencingSettings  `json:"silencing"`
	Thresholds      OpsMetricThresholds        `json:"thresholds"` // 指标阈值配置
}

// OpsAdvancedSettings stores advanced ops configuration (data retention, aggregation).
type OpsAdvancedSettings struct {
	DataRetention                   OpsDataRetentionSettings               `json:"data_retention"`
	Aggregation                     OpsAggregationSettings                 `json:"aggregation"`
	OpenAIAccountQuotaAutoPause     OpsOpenAIAccountQuotaAutoPauseSettings `json:"openai_account_quota_auto_pause"`
	AccountErrorRateMonitor         OpsAccountErrorRateMonitorSettings     `json:"account_error_rate_monitor"`
	IgnoreCountTokensErrors         bool                                   `json:"ignore_count_tokens_errors"`
	IgnoreContextCanceled           bool                                   `json:"ignore_context_canceled"`
	IgnoreNoAvailableAccounts       bool                                   `json:"ignore_no_available_accounts"`
	IgnoreInvalidApiKeyErrors       bool                                   `json:"ignore_invalid_api_key_errors"`
	IgnoreInsufficientBalanceErrors bool                                   `json:"ignore_insufficient_balance_errors"`
	DisplayOpenAITokenStats         bool                                   `json:"display_openai_token_stats"`
	DisplayAlertEvents              bool                                   `json:"display_alert_events"`
	AutoRefreshEnabled              bool                                   `json:"auto_refresh_enabled"`
	AutoRefreshIntervalSec          int                                    `json:"auto_refresh_interval_seconds"`
}

type OpsOpenAIAccountQuotaAutoPauseSettings struct {
	DefaultThreshold5h float64 `json:"default_threshold_5h"`
	DefaultThreshold7d float64 `json:"default_threshold_7d"`
}

// OpsAccountErrorRateMonitorSettings 配置「按渠道(账号)上游错误率」的分钟级巡检：
// 某个账号窗口内的上游错误率破阈值即可单独告警，并(可开关)自动把它临时移出调度列表，
// 避免单个故障渠道被整体平均掩盖、影响客户。与整体 upstream_error_rate 告警相互独立。
type OpsAccountErrorRateMonitorSettings struct {
	Enabled               bool    `json:"enabled"`                 // 总开关，默认 false
	WindowMinutes         int     `json:"window_minutes"`          // 错误率统计窗口(分钟)，默认 5
	ErrorRateThreshold    float64 `json:"error_rate_threshold"`    // 上游错误率阈值(0-100)，默认 30
	MinRequests           int     `json:"min_requests"`            // 窗口内最小请求样本，低于此值跳过判定(防小样本抖动)，默认 20
	SustainedMinutes      int     `json:"sustained_minutes"`       // 连续破阈值持续时长(分钟)，默认 0(单次即触发)
	AutoDetach            bool    `json:"auto_detach"`             // 是否自动剥离(临时下线)，默认 false(仅告警)
	DetachCooldownMinutes int     `json:"detach_cooldown_minutes"` // 自动剥离回归时长：0=保持下线需人工恢复，>0=N分钟后自愈，默认 30
	NotifyEnabled         bool    `json:"notify_enabled"`          // 是否发告警通知，默认 true
	AlertCooldownMinutes  int     `json:"alert_cooldown_minutes"`  // 同一渠道两次告警最小间隔(分钟)，默认 30
}

type OpsDataRetentionSettings struct {
	CleanupEnabled             bool   `json:"cleanup_enabled"`
	CleanupSchedule            string `json:"cleanup_schedule"`
	ErrorLogRetentionDays      int    `json:"error_log_retention_days"`
	MinuteMetricsRetentionDays int    `json:"minute_metrics_retention_days"`
	HourlyMetricsRetentionDays int    `json:"hourly_metrics_retention_days"`
}

type OpsAggregationSettings struct {
	AggregationEnabled bool `json:"aggregation_enabled"`
}

// OpsLarkNotificationConfig stores Lark (Feishu) notification settings.
type OpsLarkNotificationConfig struct {
	// Enabled is the global switch for Lark notifications.
	Enabled bool `json:"enabled"`

	// Mode is either "webhook" (default) or "app".
	Mode string `json:"mode"`

	// WebhookURL is the custom bot webhook URL (used when Mode == "webhook").
	WebhookURL string `json:"webhook_url"`

	// AppID is the Lark app ID (used when Mode == "app").
	AppID string `json:"app_id"`

	// AppSecret is the Lark app secret (used when Mode == "app").
	AppSecret string `json:"app_secret"`

	// ReceiveID is the chat_id or open_id to send messages to (used when Mode == "app").
	ReceiveID string `json:"receive_id"`

	// ReceiveIDType is "chat_id" | "open_id" | "user_id" | "union_id" (default "chat_id").
	ReceiveIDType string `json:"receive_id_type"`

	// Alert controls which alerts are pushed to Lark.
	Alert OpsLarkAlertConfig `json:"alert"`
}

// OpsLarkAlertConfig controls ops alert push behavior.
type OpsLarkAlertConfig struct {
	Enabled     bool   `json:"enabled"`
	MinSeverity string `json:"min_severity"`
}

// OpsLarkNotificationConfigUpdateRequest supports partial updates.
type OpsLarkNotificationConfigUpdateRequest struct {
	Enabled       *bool               `json:"enabled"`
	Mode          *string             `json:"mode"`
	WebhookURL    *string             `json:"webhook_url"`
	AppID         *string             `json:"app_id"`
	AppSecret     *string             `json:"app_secret"`
	ReceiveID     *string             `json:"receive_id"`
	ReceiveIDType *string             `json:"receive_id_type"`
	Alert         *OpsLarkAlertConfig `json:"alert"`
}
