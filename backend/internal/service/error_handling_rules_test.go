package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func errorHandlingIntPtr(n int) *int { return &n }

type countingSettingRepo struct {
	gatewayTTLSettingRepo
	getValueCalls int
}

func (r *countingSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.getValueCalls++
	return r.gatewayTTLSettingRepo.GetValue(ctx, key)
}

func TestMatchErrorHandlingRule(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "r1", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)},
		{ID: "r2", Keywords: []string{"prompt is too long"}, Action: ErrorHandlingActionPassthrough},
		{ID: "r3", StatusCodes: []int{400}, Keywords: []string{"deserialize"}, Action: ErrorHandlingActionFailover},
	}

	tests := []struct {
		name       string
		statusCode int
		body       string
		wantRuleID string
	}{
		{"仅状态码匹配", 429, `{"error":"rate limited"}`, "r1"},
		{"仅关键字匹配（大小写不敏感）", 500, `{"error":"Prompt Is Too Long for this model"}`, "r2"},
		{"状态码+关键字都命中(AND)才算 r3", 400, `{"error":"Failed to deserialize request body"}`, "r3"},
		{"状态码对但关键字不对，不命中", 400, `{"error":"something else"}`, ""},
		{"关键字对但状态码不对，不命中 r3", 401, `{"error":"Failed to deserialize request body"}`, ""},
		{"都不命中", 404, `{"error":"not found"}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchErrorHandlingRule(rules, tt.statusCode, []byte(tt.body))
			if tt.wantRuleID == "" {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, tt.wantRuleID, got.ID)
		})
	}
}

func TestMatchErrorHandlingRulePicksFirstMatchInOrder(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "first", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "second", StatusCodes: []int{500}, Action: ErrorHandlingActionFailover},
	}
	got := matchErrorHandlingRule(rules, 500, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "first", got.ID)
}

func TestMatchErrorHandlingRuleSkipsRuleWithNoConditions(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "empty", Action: ErrorHandlingActionRetry},
		{ID: "fallback", Keywords: []string{"boom"}, Action: ErrorHandlingActionRetry},
	}
	got := matchErrorHandlingRule(rules, 500, []byte(`{"error":"boom"}`))
	require.NotNil(t, got)
	require.Equal(t, "fallback", got.ID)
}

// 未知 action 只能靠直接改库产生（写入路径有 validate 兜着），但匹配层必须把它
// 当成「这条规则不存在」：applyErrorHandlingRule 的 switch 没有对应分支，放行等于
// 掉进 retry 代码且不消耗重试计数，变成不受次数限制的原地重试。
func TestMatchErrorHandlingRuleSkipsRuleWithUnknownAction(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "unknown", StatusCodes: []int{500}, Action: "explode"},
		{ID: "valid", StatusCodes: []int{500}, Action: ErrorHandlingActionFailover},
	}
	got := matchErrorHandlingRule(rules, 500, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "valid", got.ID)
}

func TestNormalizeErrorHandlingRulesFillsEmptyRuleID(t *testing.T) {
	rules := []ErrorHandlingRule{
		{StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
	}
	normalizeErrorHandlingRules(rules)
	require.NotEmpty(t, rules[0].ID)
	require.NotEmpty(t, rules[1].ID)
	require.NotEqual(t, rules[0].ID, rules[1].ID)
}

func TestNormalizeErrorHandlingRulesDefaultsActionToRetry(t *testing.T) {
	rules := []ErrorHandlingRule{{ID: "a", StatusCodes: []int{500}}}
	normalizeErrorHandlingRules(rules)
	require.Equal(t, ErrorHandlingActionRetry, rules[0].Action)
}

func TestNormalizeErrorHandlingRulesClearsRetryCountForNonRetryActions(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "a", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough, RetryCount: errorHandlingIntPtr(3)},
		{ID: "b", StatusCodes: []int{500}, Action: ErrorHandlingActionFailover, RetryCount: errorHandlingIntPtr(3)},
	}
	normalizeErrorHandlingRules(rules)
	require.Nil(t, rules[0].RetryCount)
	require.Nil(t, rules[1].RetryCount)
}

func TestNormalizeErrorHandlingRulesDefaultsExhaustedAction(t *testing.T) {
	rules := []ErrorHandlingRule{{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry}}
	normalizeErrorHandlingRules(rules)
	require.Equal(t, ErrorHandlingExhaustedActionDefault, rules[0].ExhaustedAction)
}

func TestValidateErrorHandlingRuleSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings *ErrorHandlingRuleSettings
		wantErr  bool
	}{
		{"合法：只有状态码", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry}}}, false},
		{"合法：只有关键字", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionPassthrough}}}, false},
		{"合法：0 次重试（命中即换号）", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(0)}}}, false},
		{"合法：耗尽后安全透传", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionRetry, ExhaustedAction: ErrorHandlingExhaustedActionPassthrough}}}, false},
		{"非法：状态码和关键字都为空", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Action: ErrorHandlingActionRetry}}}, true},
		{"非法：未知 action", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: "explode"}}}, true},
		{"非法：未知 exhausted_action", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionRetry, ExhaustedAction: "raw"}}}, true},
		{"非法：负数重试次数", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(-1)}}}, true},
		{"非法：重试次数超上限", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(5)}}}, true},
		{"非法：状态码越界", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{{ID: "a", StatusCodes: []int{99}, Action: ErrorHandlingActionRetry}}}, true},
		{"非法：DefaultRetryCount 为负", &ErrorHandlingRuleSettings{DefaultRetryCount: -1, Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionRetry}}}, true},
		{"非法：DefaultRetryCount 超上限", &ErrorHandlingRuleSettings{DefaultRetryCount: 5, Rules: []ErrorHandlingRule{{ID: "a", Keywords: []string{"x"}, Action: ErrorHandlingActionRetry}}}, true},
		// 同 ID 的两条规则会共用 errorHandlingRuleTracker 的重试计数：切到第二条时
		// 计数不重置，第二条规则拿不到自己的重试预算。
		{"非法：重复 ID", &ErrorHandlingRuleSettings{Rules: []ErrorHandlingRule{
			{ID: "dup", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
			{ID: "dup", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateErrorHandlingRuleSettings(tt.settings)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestGetErrorHandlingRuleSettingsDefaultsWhenMissing(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetErrorHandlingRuleSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Empty(t, settings.Rules)
	require.Equal(t, 1, settings.DefaultRetryCount)
}

func TestGetErrorHandlingRuleSettingsDoesNotMigrateLegacyPatterns(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyRectifierSettings: `{"enabled":true,"apikey_signature_patterns":["prompt is too long"]}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetErrorHandlingRuleSettings(context.Background())
	require.NoError(t, err)
	require.Empty(t, settings.Rules)
	require.False(t, settings.Enabled)
	require.Equal(t, `{"enabled":true,"apikey_signature_patterns":["prompt is too long"]}`, repo.data[SettingKeyRectifierSettings])
}

func TestSetErrorHandlingRuleSettingsNormalizesAndValidates(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	require.NoError(t, svc.SetErrorHandlingRuleSettings(ctx, &ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []ErrorHandlingRule{{ID: "a", StatusCodes: []int{500}}},
	}))
	got, err := svc.GetErrorHandlingRuleSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, ErrorHandlingActionRetry, got.Rules[0].Action)

	require.Error(t, svc.SetErrorHandlingRuleSettings(ctx, &ErrorHandlingRuleSettings{
		Rules: []ErrorHandlingRule{{ID: "bad", Action: ErrorHandlingActionRetry}},
	}))
	require.Error(t, svc.SetErrorHandlingRuleSettings(ctx, &ErrorHandlingRuleSettings{
		Rules: []ErrorHandlingRule{{ID: "c", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(99)}},
	}))
}

// 校验失败必须能和仓储写入失败区分开：前者是管理员填错（400），后者是服务端故障（500）。
func TestSetErrorHandlingRuleSettingsWrapsValidationError(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.SetErrorHandlingRuleSettings(context.Background(), &ErrorHandlingRuleSettings{
		Rules: []ErrorHandlingRule{{ID: "a", Action: ErrorHandlingActionRetry}},
	})
	require.ErrorIs(t, err, ErrErrorHandlingRuleSettingsInvalid)
}

func TestGetErrorHandlingRuleSettingsCachedAvoidsRepeatedDBReads(t *testing.T) {
	repo := &countingSettingRepo{gatewayTTLSettingRepo: gatewayTTLSettingRepo{data: map[string]string{}}}
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	require.NoError(t, svc.SetErrorHandlingRuleSettings(ctx, &ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []ErrorHandlingRule{{ID: "a", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}},
	}))

	before := repo.getValueCalls
	for i := 0; i < 10; i++ {
		settings := svc.GetErrorHandlingRuleSettingsCached(ctx)
		require.True(t, settings.Enabled)
		require.Len(t, settings.Rules, 1)
	}
	require.Equal(t, before, repo.getValueCalls)
}
