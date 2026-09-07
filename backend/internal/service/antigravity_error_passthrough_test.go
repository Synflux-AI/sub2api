package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bindAntigravityPassthroughRule 绑定一条 passthrough_code=true + skip_monitoring=true 的
// 错误透传规则，用于验证 antigravity compat 错误路径真的执行了该规则（而不只是让路）。
func bindAntigravityPassthroughRule(c *gin.Context) {
	rule := &model.ErrorPassthroughRule{
		ID:              1,
		Name:            "antigravity-compat-passthrough",
		Enabled:         true,
		Priority:        1,
		Platforms:       []string{PlatformAntigravity},
		ErrorCodes:      []int{http.StatusBadRequest},
		Keywords:        []string{"invalid schema"},
		MatchMode:       model.MatchModeAll,
		PassthroughCode: true,
		PassthroughBody: true,
		SkipMonitoring:  true,
	}
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, svc)
}

func TestWriteMappedAntigravityCompatError_AppliesPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	bindAntigravityPassthroughRule(c)

	svc := &AntigravityGatewayService{}
	account := newAntigravityCompatAccount(AccountTypeAPIKey)
	body := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)

	err := svc.writeMappedAntigravityCompatError(c, account, http.StatusBadRequest, "req-passthrough", body)
	require.Error(t, err)

	// (a) 最终 HTTP 状态码 == 上游状态码（passthrough_code=true）。
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)

	// (b) 响应消息 == 上游错误消息（本规则未配置 custom_message）。
	assert.Equal(t, "Invalid schema for field messages", errField["message"])
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Nil(t, errField["param"])
	assert.Nil(t, errField["code"])

	// (c) skip_monitoring 生效。
	v, exists := c.Get(OpsSkipPassthroughKey)
	require.True(t, exists, "OpsSkipPassthroughKey should be set when skip_monitoring=true")
	assert.Equal(t, true, v)
}

func TestWriteMappedAntigravityCompatError_NoRuleKeepsBuiltinMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	// 不绑定任何错误透传服务：存量行为必须与 #228 之前逐字节一致。
	svc := &AntigravityGatewayService{}
	account := newAntigravityCompatAccount(AccountTypeAPIKey)
	body := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)

	err := svc.writeMappedAntigravityCompatError(c, account, http.StatusBadRequest, "req-default", body)
	require.Error(t, err)

	assert.Equal(t, mapUpstreamStatusCode(http.StatusBadRequest), rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, "Upstream request failed", errField["message"])
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Nil(t, errField["param"])
	assert.Nil(t, errField["code"])

	_, exists := c.Get(OpsSkipPassthroughKey)
	assert.False(t, exists, "OpsSkipPassthroughKey must not be set without a bound passthrough service")
}
