package dto

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// UserAccessToken 是用户级 access token（sat-…）的面板响应视图，用户自助侧与
// 管理侧共用同一个结构：两处返回字节级相同的形状，前端可复用同一份解析与卡片。
//
// 三个字段都可为 null——用户尚未生成令牌时整体为
// {token:null,created_at:null,last_used_at:null}，前端据此渲染「未生成」状态，
// 不必区分 data 缺失与令牌缺失。
type UserAccessToken struct {
	Token      *string    `json:"token"`
	CreatedAt  *time.Time `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// UserAccessTokenFromService 把仓储记录转成视图；record 为 nil 时三个字段全为 null。
func UserAccessTokenFromService(record *service.UserAccessToken) UserAccessToken {
	if record == nil {
		return UserAccessToken{}
	}
	token := record.Token
	createdAt := record.CreatedAt
	return UserAccessToken{
		Token:      &token,
		CreatedAt:  &createdAt,
		LastUsedAt: record.LastUsedAt,
	}
}
