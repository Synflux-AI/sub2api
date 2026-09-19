package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type historyRepo struct {
	service.RedeemCodeRepository
	userID      int64
	params      pagination.PaginationParams
	legacyLimit int
}

func (r *historyRepo) ListByUser(_ context.Context, userID int64, limit int) ([]service.RedeemCode, error) {
	r.userID, r.legacyLimit = userID, limit
	return []service.RedeemCode{}, nil
}
func (r *historyRepo) ListByUserPaginated(_ context.Context, userID int64, params pagination.PaginationParams, codeType string) ([]service.RedeemCode, *pagination.PaginationResult, error) {
	r.userID, r.params = userID, params
	return []service.RedeemCode{}, &pagination.PaginationResult{Total: 101}, nil
}

// 本仓的 GetHistory 在 PR #213 里就已经改成「恒定分页」：直接走
// response.ParsePagination（宽松解析：非法值回落默认值而不是 400，上限 1000 而不是
// 100），前端 redeem.ts 也永远带 page/page_size 调它。上游 #7212 走的是另一条路线
// ——不带参数时保留旧的数组响应、带参数时严格校验并 400。合并时保留本仓行为，这里
// 把上游用例改成按本仓语义断言。
//
// 已知缺口（非本次同步引入，ParsePagination 全仓共用）：page 传超大值时
// PaginationParams.Offset() 的 (page-1)*limit 会整型溢出，上游那版的
// `page-1 > MaxInt/pageSize` 校验正是挡这个的。要修应在 ParsePagination 统一修。
func TestRedeemHistory(t *testing.T) {
	for _, tt := range []struct {
		name, query        string
		userID             int64
		status, page, size int
	}{
		{"no params falls back to first page", "", 7, 200, 1, 20},
		{"default", "?page=1", 7, 200, 1, 20},
		{"size only", "?page_size=50", 7, 200, 1, 50},
		{"second user", "?page=2&page_size=100&user_id=7", 8, 200, 2, 100},
		{"above upstream cap still accepted", "?page_size=101", 7, 200, 1, 101},
		{"above parse cap falls back to default", "?page_size=1001", 7, 200, 1, 20},
		{"beyond last", "?page=100&page_size=20", 7, 200, 100, 20},
		{"zero", "?page=0", 7, 200, 1, 20},
		{"negative", "?page_size=-1", 7, 200, 1, 20},
		{"invalid", "?page=abc", 7, 200, 1, 20},
		{"unauthenticated", "?page=1", 0, 401, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &historyRepo{}
			h := NewRedeemHandler(service.NewRedeemService(repo, nil, nil, nil, nil, nil, nil, nil))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/api/v1/redeem/history"+tt.query, nil)
			if tt.userID != 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tt.userID})
			}
			h.GetHistory(c)
			require.Equal(t, tt.status, w.Code)
			if tt.status != 200 {
				require.Zero(t, repo.userID)
				return
			}
			require.Equal(t, tt.userID, repo.userID)
			// 旧的 ListByUser(limit) 分支已经不存在了，任何入参都必须落到分页仓储上。
			require.Zero(t, repo.legacyLimit)
			var body struct {
				Data json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, tt.page, repo.params.Page)
			require.Equal(t, tt.size, repo.params.PageSize)
			var data struct {
				Items []service.RedeemCode `json:"items"`
				Total int                  `json:"total"`
				Page  int                  `json:"page"`
				Size  int                  `json:"page_size"`
			}
			require.NoError(t, json.Unmarshal(body.Data, &data))
			require.NotNil(t, data.Items)
			require.Equal(t, 101, data.Total)
			require.Equal(t, tt.page, data.Page)
			require.Equal(t, tt.size, data.Size)
		})
	}
}
