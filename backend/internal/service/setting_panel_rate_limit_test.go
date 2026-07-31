package service

import (
	"context"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type panelRateLimitSettingRepo struct {
	mu            sync.Mutex
	values        map[string]string
	getValueErr   error
	getValueCalls int
}

func (r *panelRateLimitSettingRepo) Get(_ context.Context, key string) (*Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: value}, nil
}

func (r *panelRateLimitSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getValueCalls++
	if r.getValueErr != nil {
		return "", r.getValueErr
	}
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *panelRateLimitSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func (r *panelRateLimitSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
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

func (r *panelRateLimitSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
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

func (r *panelRateLimitSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *panelRateLimitSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

func newPanelRateLimitTestService(repo SettingRepository) *SettingService {
	return NewSettingService(repo, &config.Config{})
}

func TestGetPanelRateLimitSettingsDefaults(t *testing.T) {
	svc := newPanelRateLimitTestService(&panelRateLimitSettingRepo{})

	settings, err := svc.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, DefaultPanelRateLimitSettings(), settings)
}

func TestGetPanelRateLimitSettingsInvalidJSONFallsBack(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{
		SettingKeyPanelRateLimitSettings: "{not-json",
	}}
	svc := newPanelRateLimitTestService(repo)

	settings, err := svc.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, DefaultPanelRateLimitSettings(), settings)
}

func TestGetPanelRateLimitSettingsNormalizesValues(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{
		SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":-5,"heavy_rpm":999999999,"exempt_admin":false,"public_ip_rpm":10,"open_api_rpm":-3}`,
	}}
	svc := newPanelRateLimitTestService(repo)

	settings, err := svc.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.Equal(t, 0, settings.UserRPM)
	require.Equal(t, panelRateLimitRPMMax, settings.HeavyRPM)
	require.Equal(t, 10, settings.PublicIPRPM)
	require.False(t, settings.ExemptAdmin)
	require.Equal(t, 0, settings.OpenAPIRPM, "负数在读路径归零")
}

func TestGetPanelRateLimitSettingsNormalizesOpenAPIRPMOverMax(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{
		SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":100,"heavy_rpm":20,"exempt_admin":true,"public_ip_rpm":50,"open_api_rpm":999999999}`,
	}}
	svc := newPanelRateLimitTestService(repo)

	settings, err := svc.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, panelRateLimitRPMMax, settings.OpenAPIRPM, "超上限在读路径截断到 max")
}

// 历史 JSON 缺失 open_api_rpm 字段（本次新增前写入的行）：读到默认值 60，
// 而不是零值（零值会被限流器当作「不限流」，等于新字段静默失效）。
func TestGetPanelRateLimitSettingsMissingOpenAPIRPMFallsBackToDefault(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{
		SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":100,"heavy_rpm":20,"exempt_admin":true,"public_ip_rpm":50}`,
	}}
	svc := newPanelRateLimitTestService(repo)

	settings, err := svc.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 60, settings.OpenAPIRPM, "缺字段应回落到默认值 60")
}

// 与上面相对：JSON 里显式写 0 必须保持 0（=不限流），不能被默认值预填盖掉。
func TestGetPanelRateLimitSettingsExplicitZeroOpenAPIRPMStaysZero(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{
		SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":100,"heavy_rpm":20,"exempt_admin":true,"public_ip_rpm":50,"open_api_rpm":0}`,
	}}
	svc := newPanelRateLimitTestService(repo)

	settings, err := svc.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, settings.OpenAPIRPM, "显式 0 必须保持 0，不能被默认值预填覆盖")
}

func TestSetPanelRateLimitSettingsValidation(t *testing.T) {
	svc := newPanelRateLimitTestService(&panelRateLimitSettingRepo{})

	require.Error(t, svc.SetPanelRateLimitSettings(context.Background(), nil))
	require.Error(t, svc.SetPanelRateLimitSettings(context.Background(), &PanelRateLimitSettings{UserRPM: -1}))
	require.Error(t, svc.SetPanelRateLimitSettings(context.Background(), &PanelRateLimitSettings{HeavyRPM: panelRateLimitRPMMax + 1}))
	require.Error(t, svc.SetPanelRateLimitSettings(context.Background(), &PanelRateLimitSettings{OpenAPIRPM: -1}),
		"写路径负数必须报错，不能像读路径那样静默归零")
	require.Error(t, svc.SetPanelRateLimitSettings(context.Background(), &PanelRateLimitSettings{OpenAPIRPM: panelRateLimitRPMMax + 1}),
		"写路径超上限必须报错，不能像读路径那样静默截断")
}

func TestSetPanelRateLimitSettingsRoundTripAndCacheRefresh(t *testing.T) {
	repo := &panelRateLimitSettingRepo{}
	svc := newPanelRateLimitTestService(repo)

	// 先填充缓存（默认值）
	cached := svc.GetPanelRateLimitSettingsCached(context.Background())
	require.Equal(t, *DefaultPanelRateLimitSettings(), cached)

	want := &PanelRateLimitSettings{
		Enabled:     true,
		UserRPM:     120,
		HeavyRPM:    30,
		ExemptAdmin: false,
		PublicIPRPM: 60,
		OpenAPIRPM:  45,
	}
	require.NoError(t, svc.SetPanelRateLimitSettings(context.Background(), want))

	// 写入后无需等待 TTL，缓存立即反映新值
	cached = svc.GetPanelRateLimitSettingsCached(context.Background())
	require.Equal(t, *want, cached)

	// DB 中持久化的值可直读
	stored, err := svc.GetPanelRateLimitSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, want, stored)
}

func TestGetPanelRateLimitSettingsCachedAvoidsRepeatedDBReads(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{
		SettingKeyPanelRateLimitSettings: `{"enabled":true,"user_rpm":100,"heavy_rpm":20,"exempt_admin":true,"public_ip_rpm":50}`,
	}}
	svc := newPanelRateLimitTestService(repo)

	for i := 0; i < 5; i++ {
		settings := svc.GetPanelRateLimitSettingsCached(context.Background())
		require.Equal(t, 100, settings.UserRPM)
	}

	repo.mu.Lock()
	calls := repo.getValueCalls
	repo.mu.Unlock()
	require.Equal(t, 1, calls, "TTL 内应只读一次 DB")
}
