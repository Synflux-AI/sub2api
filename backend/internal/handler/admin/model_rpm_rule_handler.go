package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// ModelRPMRuleHandler 处理模型维度 RPM 限流规则的管理端接口。
type ModelRPMRuleHandler struct {
	modelRPMRuleService *service.ModelRPMRuleService
}

// NewModelRPMRuleHandler 创建模型 RPM 规则管理端 handler。
func NewModelRPMRuleHandler(modelRPMRuleService *service.ModelRPMRuleService) *ModelRPMRuleHandler {
	return &ModelRPMRuleHandler{modelRPMRuleService: modelRPMRuleService}
}

type saveModelRPMRuleRequest struct {
	Name         string `json:"name"`
	ModelPattern string `json:"model_pattern"`
	Scope        string `json:"scope"`
	TargetType   string `json:"target_type"`
	TargetID     *int64 `json:"target_id"`
	RPMLimit     int    `json:"rpm_limit"`
	Enabled      bool   `json:"enabled"`
}

func (r *saveModelRPMRuleRequest) toInput() *service.SaveModelRPMRuleInput {
	return &service.SaveModelRPMRuleInput{
		Name:         r.Name,
		ModelPattern: r.ModelPattern,
		Scope:        r.Scope,
		TargetType:   r.TargetType,
		TargetID:     r.TargetID,
		RPMLimit:     r.RPMLimit,
		Enabled:      r.Enabled,
	}
}

// List 列出全部规则（含停用），按 id 升序。
// GET /api/v1/admin/model-rpm-rules
func (h *ModelRPMRuleHandler) List(c *gin.Context) {
	items, err := h.modelRPMRuleService.List(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

// Create 创建规则。
// POST /api/v1/admin/model-rpm-rules
func (h *ModelRPMRuleHandler) Create(c *gin.Context) {
	var req saveModelRPMRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	created, err := h.modelRPMRuleService.Create(c.Request.Context(), req.toInput())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, created)
}

// Update 全量更新规则。
// PUT /api/v1/admin/model-rpm-rules/:id
func (h *ModelRPMRuleHandler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid model rpm rule ID")
		return
	}
	var req saveModelRPMRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	updated, err := h.modelRPMRuleService.Update(c.Request.Context(), id, req.toInput())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, updated)
}

// Delete 删除规则。
// DELETE /api/v1/admin/model-rpm-rules/:id
func (h *ModelRPMRuleHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid model rpm rule ID")
		return
	}
	if err := h.modelRPMRuleService.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Model RPM rule deleted"})
}
