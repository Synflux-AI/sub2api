//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// panelRateLimitHandlerRepoStub 是一个可正常工作的内存版 SettingRepository：
// 与本包其它测试复用的 settingHandlerRepoStub 不同（它的 Set 直接 panic），
// UpdatePanelRateLimitSettings 走的是单 key 的 Get/Set（而非 GetMultiple/
// SetMultiple 的整份配置读写），所以这里需要一个真正实现 Set 的 stub。
type panelRateLimitHandlerRepoStub struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *panelRateLimitHandlerRepoStub) Get(_ context.Context, key string) (*service.Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return nil, service.ErrSettingNotFound
	}
	return &service.Setting{Key: key, Value: value}, nil
}

func (r *panelRateLimitHandlerRepoStub) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}

func (r *panelRateLimitHandlerRepoStub) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func (r *panelRateLimitHandlerRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *panelRateLimitHandlerRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *panelRateLimitHandlerRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *panelRateLimitHandlerRepoStub) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

func newPanelRateLimitHandlerTest(t *testing.T, stored map[string]string) (*SettingHandler, *panelRateLimitHandlerRepoStub) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &panelRateLimitHandlerRepoStub{values: stored}
	svc := service.NewSettingService(repo, &config.Config{})
	return NewSettingHandler(svc, nil, nil, nil, nil, nil, nil), repo
}

func doUpdatePanelRateLimitSettings(t *testing.T, h *SettingHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rawBody, err := json.Marshal(body)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/panel-rate-limit", bytes.NewReader(rawBody))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdatePanelRateLimitSettings(c)
	return rec
}

// 旧客户端只发前 5 个字段的 payload（不含 open_api_rpm）：必须保留库里已存储的值，
// 而不是把它当作缺省的 0（=不限流）静默写入。这是 OpenAPIRPM 用指针而非 int 的
// 全部意义所在。
func TestUpdatePanelRateLimitSettingsOmittedOpenAPIRPMPreservesStoredValue(t *testing.T) {
	h, _ := newPanelRateLimitHandlerTest(t, map[string]string{
		service.SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":240,"heavy_rpm":60,"exempt_admin":true,"public_ip_rpm":300,"open_api_rpm":45}`,
	})

	rec := doUpdatePanelRateLimitSettings(t, h, map[string]any{
		"enabled":       true,
		"user_rpm":      240,
		"heavy_rpm":     60,
		"exempt_admin":  true,
		"public_ip_rpm": 300,
		// open_api_rpm 故意省略，模拟 Task 4 之前的旧前端 payload
	})

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"open_api_rpm":45`, "省略字段时响应体也应回显原值，而不是 0")

	stored, err := h.settingService.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 45, stored.OpenAPIRPM, "旧 payload 不含 open_api_rpm 时不得清零已存储的值")
	require.NotEqual(t, 0, stored.OpenAPIRPM)
}

// 非 nil 且合法：新值正常写入。
func TestUpdatePanelRateLimitSettingsExplicitOpenAPIRPMOverwritesStoredValue(t *testing.T) {
	h, _ := newPanelRateLimitHandlerTest(t, map[string]string{
		service.SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":240,"heavy_rpm":60,"exempt_admin":true,"public_ip_rpm":300,"open_api_rpm":45}`,
	})

	rec := doUpdatePanelRateLimitSettings(t, h, map[string]any{
		"enabled":       true,
		"user_rpm":      240,
		"heavy_rpm":     60,
		"exempt_admin":  true,
		"public_ip_rpm": 300,
		"open_api_rpm":  90,
	})

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"open_api_rpm":90`)

	stored, err := h.settingService.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 90, stored.OpenAPIRPM)
}

// 显式发送 0：与"省略"不同，视为主动关闭限流，必须真正写成 0。
func TestUpdatePanelRateLimitSettingsExplicitZeroOpenAPIRPMDisablesLimit(t *testing.T) {
	h, _ := newPanelRateLimitHandlerTest(t, map[string]string{
		service.SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":240,"heavy_rpm":60,"exempt_admin":true,"public_ip_rpm":300,"open_api_rpm":45}`,
	})

	rec := doUpdatePanelRateLimitSettings(t, h, map[string]any{
		"enabled":       true,
		"user_rpm":      240,
		"heavy_rpm":     60,
		"exempt_admin":  true,
		"public_ip_rpm": 300,
		"open_api_rpm":  0,
	})

	require.Equal(t, http.StatusOK, rec.Code)

	stored, err := h.settingService.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, stored.OpenAPIRPM)
}

// 非 nil 负数：走既有写路径校验 → 400，且不得污染已存储的值。
func TestUpdatePanelRateLimitSettingsNegativeOpenAPIRPMRejected(t *testing.T) {
	h, _ := newPanelRateLimitHandlerTest(t, map[string]string{
		service.SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":240,"heavy_rpm":60,"exempt_admin":true,"public_ip_rpm":300,"open_api_rpm":45}`,
	})

	rec := doUpdatePanelRateLimitSettings(t, h, map[string]any{
		"enabled":       true,
		"user_rpm":      240,
		"heavy_rpm":     60,
		"exempt_admin":  true,
		"public_ip_rpm": 300,
		"open_api_rpm":  -1,
	})

	require.Equal(t, http.StatusBadRequest, rec.Code)

	stored, err := h.settingService.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 45, stored.OpenAPIRPM, "校验失败不得改动已存储的值")
}

// 超上限同理走既有写路径校验 → 400。
func TestUpdatePanelRateLimitSettingsOverMaxOpenAPIRPMRejected(t *testing.T) {
	h, _ := newPanelRateLimitHandlerTest(t, map[string]string{
		service.SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":240,"heavy_rpm":60,"exempt_admin":true,"public_ip_rpm":300,"open_api_rpm":45}`,
	})

	rec := doUpdatePanelRateLimitSettings(t, h, map[string]any{
		"enabled":       true,
		"user_rpm":      240,
		"heavy_rpm":     60,
		"exempt_admin":  true,
		"public_ip_rpm": 300,
		"open_api_rpm":  100001,
	})

	require.Equal(t, http.StatusBadRequest, rec.Code)

	stored, err := h.settingService.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 45, stored.OpenAPIRPM)
}

// 省略字段且库里此前也没有存过任何配置行：应回落到默认值 60。
func TestUpdatePanelRateLimitSettingsOmittedOpenAPIRPMDefaultsWhenNoStoredRow(t *testing.T) {
	h, _ := newPanelRateLimitHandlerTest(t, map[string]string{})

	rec := doUpdatePanelRateLimitSettings(t, h, map[string]any{
		"enabled":       true,
		"user_rpm":      240,
		"heavy_rpm":     60,
		"exempt_admin":  true,
		"public_ip_rpm": 300,
	})

	require.Equal(t, http.StatusOK, rec.Code)

	stored, err := h.settingService.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 60, stored.OpenAPIRPM)
}
