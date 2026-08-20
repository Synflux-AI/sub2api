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
			// 跟 SetErrorHandlingRuleSettings 一样先 normalize：validate 的前置条件是
			// 默认值已补齐（例如 priority 已从 0 补成 1..N），单独调用会误判。
			normalizeErrorHandlingRules(tt.settings.Rules)
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

func errorHandlingBoolPtr(b bool) *bool { return &b }

func TestMatchErrorHandlingRuleSkipsDisabledRule(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "disabled", Enabled: errorHandlingBoolPtr(false), StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "enabled", Enabled: errorHandlingBoolPtr(true), StatusCodes: []int{500}, Action: ErrorHandlingActionFailover},
	}
	got := matchErrorHandlingRule(rules, 500, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "enabled", got.ID)
}

// 存量配置没有 enabled 字段，反序列化后是 nil，必须照常参与匹配 —— 否则线上所有
// 错误处理规则会在升级瞬间静默失效。
func TestMatchErrorHandlingRuleTreatsNilEnabledAsEnabled(t *testing.T) {
	rules := []ErrorHandlingRule{{ID: "legacy", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}}
	got := matchErrorHandlingRule(rules, 500, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "legacy", got.ID)
}

func TestMatchErrorHandlingRuleReturnsNilWhenAllRulesDisabled(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "a", Enabled: errorHandlingBoolPtr(false), StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "b", Enabled: errorHandlingBoolPtr(false), Keywords: []string{"boom"}, Action: ErrorHandlingActionFailover},
	}
	require.Nil(t, matchErrorHandlingRule(rules, 500, []byte(`{"error":"boom"}`)))
}

// 回归风险在 JSON 层，所以直接喂一段不含 enabled 字段的存量配置走完整反序列化路径。
func TestGetErrorHandlingRuleSettingsTreatsLegacyRulesAsEnabled(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyErrorHandlingRules: `{"enabled":true,"default_retry_count":1,"rules":[{"id":"legacy","name":"Legacy","status_codes":[429],"action":"retry"}]}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetErrorHandlingRuleSettings(context.Background())
	require.NoError(t, err)
	require.Len(t, settings.Rules, 1)
	require.Nil(t, settings.Rules[0].Enabled)
	require.True(t, settings.Rules[0].IsEnabled())
	require.True(t, HasEnabledErrorHandlingRule(settings.Rules))
}

func TestSetErrorHandlingRuleSettingsRoundTripsDisabledFlag(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	require.NoError(t, svc.SetErrorHandlingRuleSettings(ctx, &ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []ErrorHandlingRule{{ID: "a", Enabled: errorHandlingBoolPtr(false), StatusCodes: []int{500}, Action: ErrorHandlingActionRetry}},
	}))

	got, err := svc.GetErrorHandlingRuleSettings(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Rules[0].Enabled)
	require.False(t, *got.Rules[0].Enabled)
	require.False(t, got.Rules[0].IsEnabled())
}

func TestHasEnabledErrorHandlingRule(t *testing.T) {
	require.False(t, HasEnabledErrorHandlingRule(nil))
	require.False(t, HasEnabledErrorHandlingRule([]ErrorHandlingRule{
		{ID: "a", Enabled: errorHandlingBoolPtr(false), StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
	}))
	require.True(t, HasEnabledErrorHandlingRule([]ErrorHandlingRule{
		{ID: "a", Enabled: errorHandlingBoolPtr(false), StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "b", StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
	}))
}

// 回归风险在 JSON 层：存量库里的配置根本没有 priority 字段，反序列化后全是 0。
// 必须按数组下标补成 1..N，才能保证升级后的匹配顺序和升级前逐条一致（无需数据迁移）。
func TestGetErrorHandlingRuleSettingsBackfillsLegacyPriorityByIndex(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyErrorHandlingRules: `{"enabled":true,"default_retry_count":1,"rules":[` +
			`{"id":"legacy-1","status_codes":[429],"action":"retry"},` +
			`{"id":"legacy-2","status_codes":[500],"action":"failover"},` +
			`{"id":"legacy-3","keywords":["boom"],"action":"passthrough"}` +
			`]}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetErrorHandlingRuleSettings(context.Background())
	require.NoError(t, err)
	require.Len(t, settings.Rules, 3)
	require.Equal(t, []string{"legacy-1", "legacy-2", "legacy-3"},
		[]string{settings.Rules[0].ID, settings.Rules[1].ID, settings.Rules[2].ID},
		"补默认值不能打乱存量顺序")
	require.Equal(t, 1, settings.Rules[0].Priority)
	require.Equal(t, 2, settings.Rules[1].Priority)
	require.Equal(t, 3, settings.Rules[2].Priority)
}

// 「数字越小越先匹配」的语义由 normalize 层排序落实，matchErrorHandlingRule 仍是
// 「按数组顺序取首个命中」，所以必须验证 normalize 后数组已经是优先级序。
func TestNormalizeErrorHandlingRulesSortsByPriorityAscending(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "p30", Priority: 30, StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "p10", Priority: 10, StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough},
		{ID: "p20", Priority: 20, StatusCodes: []int{500}, Action: ErrorHandlingActionFailover},
	}
	normalizeErrorHandlingRules(rules)

	require.Equal(t, []int{10, 20, 30}, []int{rules[0].Priority, rules[1].Priority, rules[2].Priority})
	require.Equal(t, []string{"p10", "p20", "p30"}, []string{rules[0].ID, rules[1].ID, rules[2].ID})

	got := matchErrorHandlingRule(rules, 500, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "p10", got.ID, "三条都能命中 500 时，priority 最小的那条胜出")
}

// 相同 priority 是允许的（前端不强制去重）。用稳定排序保证它们维持提交顺序，
// 后端匹配顺序才能和前端列表展示顺序一模一样。
func TestNormalizeErrorHandlingRulesKeepsSubmitOrderForEqualPriority(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "first", Priority: 5, StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "second", Priority: 5, StatusCodes: []int{500}, Action: ErrorHandlingActionFailover},
	}
	normalizeErrorHandlingRules(rules)

	require.Equal(t, []string{"first", "second"}, []string{rules[0].ID, rules[1].ID})
	got := matchErrorHandlingRule(rules, 500, []byte(`{}`))
	require.NotNil(t, got)
	require.Equal(t, "first", got.ID)
}

// 0 是「字段缺失」的零值，负数只能由手改库/异常客户端产生：两者都按下标补默认，
// 避免带着非法值进入排序和匹配。
func TestNormalizeErrorHandlingRulesFillsNonPositivePriority(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "zero", Priority: 0, StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "negative", Priority: -1, StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
	}
	normalizeErrorHandlingRules(rules)

	require.Equal(t, 1, rules[0].Priority)
	require.Equal(t, 2, rules[1].Priority)
	require.Equal(t, []string{"zero", "negative"}, []string{rules[0].ID, rules[1].ID})
}

// 越界的 priority 属于「管理员填错了」，必须走 400 那条错误分支而不是被静默接受。
func TestSetErrorHandlingRuleSettingsRejectsPriorityAboveCap(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.SetErrorHandlingRuleSettings(context.Background(), &ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []ErrorHandlingRule{
			{ID: "a", Priority: errorHandlingRulePriorityMax + 1, StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
		},
	})
	require.ErrorIs(t, err, ErrErrorHandlingRuleSettingsInvalid)
}

func TestSetErrorHandlingRuleSettingsAcceptsPriorityAtCap(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	require.NoError(t, svc.SetErrorHandlingRuleSettings(ctx, &ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []ErrorHandlingRule{
			{ID: "a", Priority: errorHandlingRulePriorityMax, StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
		},
	}))
	got, err := svc.GetErrorHandlingRuleSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, errorHandlingRulePriorityMax, got.Rules[0].Priority)
}

// ID 是 errorHandlingRuleTracker 记重试计数的键。占位 ID 必须在排序前按原下标算，
// 且已有 ID 要跟着规则一起搬家 —— 否则管理员改一次优先级就换一次 ID，重试预算串号。
func TestNormalizeErrorHandlingRulesKeepsRuleIDWhenPriorityReorders(t *testing.T) {
	rules := []ErrorHandlingRule{
		{ID: "keep-me", Priority: 9, StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{ID: "and-me", Priority: 2, StatusCodes: []int{429}, Action: ErrorHandlingActionFailover},
	}
	normalizeErrorHandlingRules(rules)

	require.Equal(t, "and-me", rules[0].ID)
	require.Equal(t, []int{429}, rules[0].StatusCodes)
	require.Equal(t, "keep-me", rules[1].ID)
	require.Equal(t, []int{500}, rules[1].StatusCodes)
}

// 占位 ID 按排序前的下标生成：先补 rule-1/rule-2，再按 priority 重排，
// 于是 rule-2 排到了前面。ID 与下标解绑，改优先级不会重算 ID。
func TestNormalizeErrorHandlingRulesAssignsPlaceholderIDBeforeSorting(t *testing.T) {
	rules := []ErrorHandlingRule{
		{Priority: 20, StatusCodes: []int{500}, Action: ErrorHandlingActionRetry},
		{Priority: 10, StatusCodes: []int{429}, Action: ErrorHandlingActionRetry},
	}
	normalizeErrorHandlingRules(rules)

	require.Equal(t, "rule-2", rules[0].ID)
	require.Equal(t, 10, rules[0].Priority)
	require.Equal(t, "rule-1", rules[1].ID)
	require.Equal(t, 20, rules[1].Priority)
}
