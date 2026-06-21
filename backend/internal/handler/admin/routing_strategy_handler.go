package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RoutingStrategyHandler 处理智能路由策略的管理端接口。
type RoutingStrategyHandler struct {
	routingStrategyService *service.RoutingStrategyService
}

// NewRoutingStrategyHandler 创建路由策略管理端 handler。
func NewRoutingStrategyHandler(routingStrategyService *service.RoutingStrategyService) *RoutingStrategyHandler {
	return &RoutingStrategyHandler{routingStrategyService: routingStrategyService}
}

type routingConditionRequest struct {
	Type  string `json:"type"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

type saveRoutingStrategyRequest struct {
	Name              string                    `json:"name" binding:"required"`
	Description       string                    `json:"description"`
	Enabled           bool                      `json:"enabled"`
	Priority          int                       `json:"priority"`
	Platform          string                    `json:"platform"`
	GroupID           *int64                    `json:"group_id"`
	MatchMode         string                    `json:"match_mode"`
	Conditions        []routingConditionRequest `json:"conditions"`
	Action            string                    `json:"action"`
	AccountIDs        []int64                   `json:"account_ids"`
	AccountPriorities []int                     `json:"account_priorities"`
}

func (r *saveRoutingStrategyRequest) toInput() *service.SaveRoutingStrategyInput {
	conditions := make([]service.RoutingCondition, 0, len(r.Conditions))
	for i := range r.Conditions {
		conditions = append(conditions, service.RoutingCondition{
			Type:  r.Conditions[i].Type,
			Op:    r.Conditions[i].Op,
			Value: r.Conditions[i].Value,
		})
	}
	return &service.SaveRoutingStrategyInput{
		Name:              r.Name,
		Description:       r.Description,
		Enabled:           r.Enabled,
		Priority:          r.Priority,
		Platform:          r.Platform,
		GroupID:           r.GroupID,
		MatchMode:         r.MatchMode,
		Conditions:        conditions,
		Action:            r.Action,
		AccountIDs:        r.AccountIDs,
		AccountPriorities: r.AccountPriorities,
	}
}

type testRoutingStrategyRequest struct {
	Platform  string `json:"platform"`
	GroupID   *int64 `json:"group_id"`
	Model     string `json:"model"`
	Client    string `json:"client"`
	UserAgent string `json:"user_agent"`
}

// List 列出所有路由策略（按 priority 升序）。
// GET /api/v1/admin/routing-strategies
func (h *RoutingStrategyHandler) List(c *gin.Context) {
	items, err := h.routingStrategyService.List(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

// GetByID 获取单个路由策略。
// GET /api/v1/admin/routing-strategies/:id
func (h *RoutingStrategyHandler) GetByID(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid routing strategy ID")
		return
	}
	item, err := h.routingStrategyService.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

// Create 创建路由策略。
// POST /api/v1/admin/routing-strategies
func (h *RoutingStrategyHandler) Create(c *gin.Context) {
	var req saveRoutingStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	created, err := h.routingStrategyService.Create(c.Request.Context(), req.toInput())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, created)
}

// Update 全量更新路由策略。
// PUT /api/v1/admin/routing-strategies/:id
func (h *RoutingStrategyHandler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid routing strategy ID")
		return
	}
	var req saveRoutingStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	updated, err := h.routingStrategyService.Update(c.Request.Context(), id, req.toInput())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, updated)
}

// Delete 删除路由策略。
// DELETE /api/v1/admin/routing-strategies/:id
func (h *RoutingStrategyHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid routing strategy ID")
		return
	}
	if err := h.routingStrategyService.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Routing strategy deleted successfully"})
}

// Test 对给定请求属性进行 dry-run 评估，返回命中的策略与决策。
// POST /api/v1/admin/routing-strategies/test
func (h *RoutingStrategyHandler) Test(c *gin.Context) {
	var req testRoutingStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	clientType := req.Client
	if clientType == "" {
		clientType = service.RoutingClientOther
	}
	mc := service.RoutingMatchContext{
		Platform:   req.Platform,
		GroupID:    req.GroupID,
		Model:      req.Model,
		ClientType: clientType,
		UserAgent:  req.UserAgent,
	}
	dec := h.routingStrategyService.TestRoutingDecision(c.Request.Context(), mc)

	action := ""
	accountIDs := []int64{}
	if len(dec.RestrictIDs) > 0 {
		action = service.RoutingActionRestrict
		accountIDs = dec.RestrictIDs
	} else if len(dec.PreferIDs) > 0 {
		action = service.RoutingActionPrefer
		accountIDs = dec.PreferIDs
	}

	accountPriorities := make([]int, len(accountIDs))
	for i, id := range accountIDs {
		accountPriorities[i] = dec.AccountPriorities[id]
	}

	response.Success(c, gin.H{
		"matched":            dec.HasMatch(),
		"strategy_id":        dec.MatchedID,
		"strategy_name":      dec.MatchedName,
		"action":             action,
		"account_ids":        accountIDs,
		"account_priorities": accountPriorities,
	})
}
