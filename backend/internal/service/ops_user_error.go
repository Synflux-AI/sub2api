package service

import "time"

// UserErrorRequest 是面向终端用户的"错误请求"精简脱敏视图（白名单）。
// 注：message（网关标准化错误描述）与 key_name（用户自有 API Key 名称）经产品决策对该用户开放；
// client_ip / user_agent 供用户定位自身请求来源；
// error_body 仅在详情接口（GetUserErrorRequestDetail）按归属校验后返回。
type UserErrorRequest struct {
	ID              int64     `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	Model           string    `json:"model"`
	InboundEndpoint string    `json:"inbound_endpoint"`
	StatusCode      int       `json:"status_code"`
	Category        string    `json:"category"`
	Platform        string    `json:"platform"`
	Message         string    `json:"message"`
	KeyName    string  `json:"key_name"`
	KeyDeleted bool    `json:"key_deleted"`
	ClientIP   *string `json:"client_ip,omitempty"`
	UserAgent  string  `json:"user_agent,omitempty"`
}

// UserErrorRequestList 是用户错误请求分页结果。
type UserErrorRequestList struct {
	Items    []*UserErrorRequest `json:"items"`
	Total    int                 `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

// MapUserErrorCategory 把后端 error_phase + error_type 映射为用户侧粗分类码。
// 返回的是稳定的分类 code（前端做 i18n），不是展示文案。
func MapUserErrorCategory(phase, errType string) string {
	switch phase {
	case "auth":
		return "auth"
	case "routing":
		return "service_unavailable"
	case "upstream", "network":
		return "upstream"
	case "internal":
		return "internal"
	case "request":
		switch errType {
		case "rate_limit_error":
			return "rate_limit"
		case "billing_error", "subscription_error":
			return "quota"
		case "invalid_request_error":
			return "invalid_request"
		}
	}
	return "other"
}

// CategoryToFilter 把用户侧分类码反向映射为后端过滤条件（plain ANY）。
// 未知分类返回两个空切片（即不施加分类过滤）。
// 注意："other" 与未知分类都走 default 返回空切片——"other" 无对应的 phase/type 组合，无法精确反查，因此等价于不过滤。
func CategoryToFilter(category string) (phases []string, errorTypes []string) {
	switch category {
	case "auth":
		return []string{"auth"}, nil
	case "service_unavailable":
		return []string{"routing"}, nil
	case "upstream":
		return []string{"upstream", "network"}, nil
	case "internal":
		return []string{"internal"}, nil
	case "rate_limit":
		return nil, []string{"rate_limit_error"}
	case "quota":
		return nil, []string{"billing_error", "subscription_error"}
	case "invalid_request":
		return nil, []string{"invalid_request_error"}
	default:
		return nil, nil
	}
}

// ToUserErrorRequest 把内部 OpsErrorLog 裁剪为用户安全视图。
func ToUserErrorRequest(e *OpsErrorLog) *UserErrorRequest {
	if e == nil {
		return nil
	}
	model := e.RequestedModel
	if model == "" {
		model = e.Model
	}
	return &UserErrorRequest{
		ID:              e.ID,
		CreatedAt:       e.CreatedAt,
		Model:           model,
		InboundEndpoint: e.InboundEndpoint,
		StatusCode:      e.StatusCode,
		Category:        MapUserErrorCategory(e.Phase, e.Type),
		Platform:        e.Platform,
		Message:         e.Message,
		KeyName:    e.APIKeyName,
		KeyDeleted: e.APIKeyDeleted,
		ClientIP:   e.ClientIP,
		UserAgent:  e.UserAgent,
	}
}

// UserErrorRequestDetail 是错误请求详情的脱敏视图(点击单行查看)。
// 在 UserErrorRequest 基础上额外暴露 error_body、upstream_status_code 与 user_agent。
type UserErrorRequestDetail struct {
	UserErrorRequest
	ErrorBody          string `json:"error_body"`
	UpstreamStatusCode *int   `json:"upstream_status_code,omitempty"`
}

// ToUserErrorRequestDetail 把内部 OpsErrorLogDetail 裁剪为用户安全详情视图。
func ToUserErrorRequestDetail(e *OpsErrorLogDetail) *UserErrorRequestDetail {
	if e == nil {
		return nil
	}
	base := ToUserErrorRequest(&e.OpsErrorLog)
	return &UserErrorRequestDetail{
		UserErrorRequest:   *base,
		ErrorBody:          e.ErrorBody,
		UpstreamStatusCode: e.UpstreamStatusCode,
	}
}
