package service

import "time"

// Ops alert rule/event models.
//
// NOTE: These are admin-facing DTOs and intentionally keep JSON naming aligned
// with the existing ops dashboard frontend (backup style).

const (
	OpsAlertStatusFiring         = "firing"
	OpsAlertStatusResolved       = "resolved"
	OpsAlertStatusManualResolved = "manual_resolved"
)

// OpsAccountErrorRateRow 是「按渠道(账号)上游错误率」巡检的单账号聚合结果。
// Requests 为窗口内请求总数(成功 usage_logs + SLA 错误),作为错误率分母;
// UpstreamErrors 为上游错误数(error_owner=provider 且非业务限流且非 429/529),口径与
// 整体 upstream_error_rate 完全一致。ErrorRate() 返回百分比(0-100)。
//
// 不含分组维度:账号与分组为多对多(account_groups),单账号没有唯一 group_id;
// 巡检按全局单一阈值遍历所有渠道,无需分组归属。
type OpsAccountErrorRateRow struct {
	AccountID      int64
	AccountName    string
	Platform       string
	Requests       int64
	UpstreamErrors int64
}

// ErrorRate 返回该账号窗口内的上游错误率百分比(0-100);无请求样本时返回 0。
func (r OpsAccountErrorRateRow) ErrorRate() float64 {
	if r.Requests <= 0 {
		return 0
	}
	return float64(r.UpstreamErrors) / float64(r.Requests) * 100
}

type OpsAlertRule struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	Enabled  bool   `json:"enabled"`
	Severity string `json:"severity"`

	MetricType string  `json:"metric_type"`
	Operator   string  `json:"operator"`
	Threshold  float64 `json:"threshold"`

	WindowMinutes    int `json:"window_minutes"`
	SustainedMinutes int `json:"sustained_minutes"`
	CooldownMinutes  int `json:"cooldown_minutes"`

	NotifyEmail bool `json:"notify_email"`
	NotifyLark  bool `json:"notify_lark"`

	Filters map[string]any `json:"filters,omitempty"`

	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type OpsAlertEvent struct {
	ID       int64  `json:"id"`
	RuleID   int64  `json:"rule_id"`
	Severity string `json:"severity"`
	Status   string `json:"status"`

	Title       string `json:"title"`
	Description string `json:"description"`

	MetricValue    *float64 `json:"metric_value,omitempty"`
	ThresholdValue *float64 `json:"threshold_value,omitempty"`

	Dimensions map[string]any `json:"dimensions,omitempty"`

	FiredAt    time.Time  `json:"fired_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`

	EmailSent bool      `json:"email_sent"`
	LarkSent  bool      `json:"lark_sent"`
	CreatedAt time.Time `json:"created_at"`
}

type OpsAlertSilence struct {
	ID int64 `json:"id"`

	RuleID   int64   `json:"rule_id"`
	Platform string  `json:"platform"`
	GroupID  *int64  `json:"group_id,omitempty"`
	Region   *string `json:"region,omitempty"`

	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`

	CreatedBy *int64    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type OpsAlertEventFilter struct {
	Limit int

	// Cursor pagination (descending by fired_at, then id).
	BeforeFiredAt *time.Time
	BeforeID      *int64

	// Optional filters.
	Status    string
	Severity  string
	EmailSent *bool

	StartTime *time.Time
	EndTime   *time.Time

	// Dimensions filters (best-effort).
	Platform string
	GroupID  *int64
}
