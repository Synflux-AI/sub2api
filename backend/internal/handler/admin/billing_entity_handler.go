package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) SetBillingEntityService(billingEntityService *service.BillingEntityService) {
	h.billingEntityService = billingEntityService
}

func (h *SettingHandler) ListBillingEntities(c *gin.Context) {
	entities, err := h.billingEntityService.List(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, entities)
}

func (h *SettingHandler) CreateBillingEntity(c *gin.Context) {
	var input service.CreateBillingEntityInput
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	entity, err := h.billingEntityService.Create(c.Request.Context(), input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, entity)
}

func (h *SettingHandler) UpdateBillingEntity(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid billing entity ID")
		return
	}
	var input service.UpdateBillingEntityInput
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	entity, err := h.billingEntityService.Update(c.Request.Context(), id, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, entity)
}

func (h *SettingHandler) DeleteBillingEntity(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid billing entity ID")
		return
	}
	if err := h.billingEntityService.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Billing entity deleted"})
}
