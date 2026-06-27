package service

import "time"

type OpsThroughputTrendPoint struct {
	BucketStart   time.Time `json:"bucket_start"`
	RequestCount  int64     `json:"request_count"`
	TokenConsumed int64     `json:"token_consumed"`
	SwitchCount   int64     `json:"switch_count"`
	QPS           float64   `json:"qps"`
	TPS           float64   `json:"tps"`
}

type OpsThroughputPlatformBreakdownItem struct {
	Platform      string `json:"platform"`
	RequestCount  int64  `json:"request_count"`
	TokenConsumed int64  `json:"token_consumed"`
}

type OpsThroughputGroupBreakdownItem struct {
	GroupID       int64  `json:"group_id"`
	GroupName     string `json:"group_name"`
	RequestCount  int64  `json:"request_count"`
	TokenConsumed int64  `json:"token_consumed"`
}

type OpsThroughputTrendResponse struct {
	Bucket string `json:"bucket"`

	Points []*OpsThroughputTrendPoint `json:"points"`

	// Optional drilldown helpers:
	// - When no platform/group is selected: returns totals by platform.
	// - When platform is selected but group is not: returns top groups in that platform.
	ByPlatform []*OpsThroughputPlatformBreakdownItem `json:"by_platform,omitempty"`
	TopGroups  []*OpsThroughputGroupBreakdownItem    `json:"top_groups,omitempty"`
}

type OpsErrorTrendPoint struct {
	BucketStart time.Time `json:"bucket_start"`

	ErrorCountTotal      int64 `json:"error_count_total"`
	BusinessLimitedCount int64 `json:"business_limited_count"`
	ErrorCountSLA        int64 `json:"error_count_sla"`

	UpstreamErrorCountExcl429529 int64 `json:"upstream_error_count_excl_429_529"`
	Upstream429Count             int64 `json:"upstream_429_count"`
	Upstream529Count             int64 `json:"upstream_529_count"`
}

type OpsErrorTrendResponse struct {
	Bucket string                `json:"bucket"`
	Points []*OpsErrorTrendPoint `json:"points"`
}

type OpsErrorDistributionItem struct {
	StatusCode      int   `json:"status_code"`
	Total           int64 `json:"total"`
	SLA             int64 `json:"sla"`
	BusinessLimited int64 `json:"business_limited"`
}

type OpsErrorDistributionResponse struct {
	Total int64                       `json:"total"`
	Items []*OpsErrorDistributionItem `json:"items"`
}

// OpsErrorBreakdownItem 是某维度取值的错误聚合行。
type OpsErrorBreakdownItem struct {
	Key             string `json:"key"`
	Label           string `json:"label"`
	Total           int64  `json:"total"`
	SLA             int64  `json:"sla"`
	BusinessLimited int64  `json:"business_limited"`
}

// OpsErrorBreakdownResponse 是 error-breakdown 接口的响应。
// Total/SLA/BusinessLimited 为全量(未被 LIMIT 截断)合计，供前端算「其它」与占比。
type OpsErrorBreakdownResponse struct {
	Dimension       string                   `json:"dimension"`
	Total           int64                    `json:"total"`
	SLA             int64                    `json:"sla"`
	BusinessLimited int64                    `json:"business_limited"`
	Items           []*OpsErrorBreakdownItem `json:"items"`
}

// OpsErrorTrendByDimPoint 是「某时间桶 × 某维度键」的错误计数（含其它桶 key='__others__'）。
type OpsErrorTrendByDimPoint struct {
	BucketStart     time.Time `json:"bucket_start"`
	Key             string    `json:"key"`
	Label           string    `json:"label"`
	Total           int64     `json:"total"`
	SLA             int64     `json:"sla"`
	BusinessLimited int64     `json:"business_limited"`
}

// OpsErrorTrendByDimResponse 是 error-trend-by-dim 接口响应。
// Points 为长表（桶×键），前端据此同时派生堆叠图与排行（单一数据源）。
type OpsErrorTrendByDimResponse struct {
	Dimension string                     `json:"dimension"`
	Bucket    string                     `json:"bucket"`
	Points    []*OpsErrorTrendByDimPoint `json:"points"`
}
