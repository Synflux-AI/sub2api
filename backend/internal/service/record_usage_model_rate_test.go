//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 两条网关的最终扣费都必须按计费模型解析倍率，并在用量业务事件里带出来源层。

func useGatewayUserGroupRateRepo(svc *GatewayService, repo UserGroupRateRepository) {
	svc.userGroupRateRepo = repo
	svc.userGroupRateResolver = newUserGroupRateResolver(repo, nil, time.Minute, nil, "service.gateway.test")
}

func TestOpenAIGatewayServiceRecordUsage_GroupModelRateBeatsUserRate(t *testing.T) {
	groupID := int64(21)
	userRate := 1.8
	usage := OpenAIUsage{InputTokens: 15, OutputTokens: 4}

	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	userRepo := &openAIRecordUsageUserRepoStub{}
	rateRepo := &userGroupModelRateRepoStub{cfg: &UserGroupRateConfig{RateMultiplier: &userRate}}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, rateRepo)

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "resp_group_model_rate", Usage: usage, Model: "gpt-5.1", Duration: time.Second},
		APIKey: &APIKey{ID: 1001, GroupID: i64p(groupID), Group: &Group{
			ID:                   groupID,
			RateMultiplier:       1.4,
			ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"gpt-5.1"}, RateMultiplier: 0.7}},
		}},
		User:    &User{ID: 2001},
		Account: &Account{ID: 3001},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, 0.7, usageRepo.lastLog.RateMultiplier)
	require.Equal(t, string(BillingRateSourceGroupModel), usageRepo.lastLog.RateSource)
	expected := expectedOpenAICost(t, svc, "gpt-5.1", usage, 0.7)
	require.InDelta(t, expected.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
	require.InDelta(t, expected.ActualCost, userRepo.lastAmount, 1e-12)
}

func TestOpenAIGatewayServiceRecordUsage_UserModelOverride(t *testing.T) {
	groupID := int64(22)
	userRate := 1.8
	usage := OpenAIUsage{InputTokens: 15, OutputTokens: 4}

	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	rateRepo := &userGroupModelRateRepoStub{cfg: &UserGroupRateConfig{
		RateMultiplier:       &userRate,
		ModelRateMultipliers: map[string]float64{"gpt-5*": 0.9},
	}}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, rateRepo)
	group := &Group{
		ID:                   groupID,
		RateMultiplier:       1.4,
		ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"gpt-5*"}, RateMultiplier: 0.7}},
	}

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result:  &OpenAIForwardResult{RequestID: "resp_user_model_rate", Usage: usage, Model: "gpt-5.1", Duration: time.Second},
		APIKey:  &APIKey{ID: 1002, GroupID: i64p(groupID), Group: group},
		User:    &User{ID: 2002},
		Account: &Account{ID: 3002},
	})
	require.NoError(t, err)
	require.Equal(t, 0.9, usageRepo.lastLog.RateMultiplier)
	require.Equal(t, string(BillingRateSourceUserModel), usageRepo.lastLog.RateSource)
	expected := expectedOpenAICost(t, svc, "gpt-5.1", usage, 0.9)
	require.InDelta(t, expected.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)

	// 未命中模型规则时回落用户专属倍率。
	err = svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result:  &OpenAIForwardResult{RequestID: "resp_user_rate_fallback", Usage: usage, Model: "gpt-4o", Duration: time.Second},
		APIKey:  &APIKey{ID: 1002, GroupID: i64p(groupID), Group: group},
		User:    &User{ID: 2002},
		Account: &Account{ID: 3002},
	})
	require.NoError(t, err)
	require.Equal(t, userRate, usageRepo.lastLog.RateMultiplier)
	require.Equal(t, string(BillingRateSourceUser), usageRepo.lastLog.RateSource)
}

func TestOpenAIGatewayServiceRecordUsage_NoModelRulesKeepsExistingRate(t *testing.T) {
	groupID := int64(23)
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, &userGroupModelRateRepoStub{})

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result:  &OpenAIForwardResult{RequestID: "resp_no_model_rules", Usage: OpenAIUsage{InputTokens: 15, OutputTokens: 4}, Model: "gpt-5.1", Duration: time.Second},
		APIKey:  &APIKey{ID: 1003, GroupID: i64p(groupID), Group: &Group{ID: groupID, RateMultiplier: 1.4}},
		User:    &User{ID: 2003},
		Account: &Account{ID: 3003},
	})
	require.NoError(t, err)
	require.Equal(t, 1.4, usageRepo.lastLog.RateMultiplier)
	require.Equal(t, string(BillingRateSourceGroup), usageRepo.lastLog.RateSource)
}

func TestGatewayServiceRecordUsage_ModelRateFollowsChannelMappedBillingModel(t *testing.T) {
	groupID := int64(31)
	userRate := 0.5
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	userRepo := &openAIRecordUsageUserRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{})
	useGatewayUserGroupRateRepo(svc, &userGroupModelRateRepoStub{cfg: &UserGroupRateConfig{RateMultiplier: &userRate}})
	usage := ClaudeUsage{InputTokens: 10, OutputTokens: 6}

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:     "gateway_model_rate_mapped",
			Usage:         usage,
			Model:         "claude-sonnet-4",
			UpstreamModel: "claude-sonnet-4",
			Duration:      time.Second,
		},
		APIKey: &APIKey{ID: 502, GroupID: &groupID, Group: &Group{
			ID:                   groupID,
			RateMultiplier:       0.8,
			ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"claude-sonnet-4"}, RateMultiplier: 0.6}},
		}},
		User:    &User{ID: 602},
		Account: &Account{ID: 702},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, 0.6, usageRepo.lastLog.RateMultiplier, "group model rate is inherited even when the user has an exclusive rate")
	require.Equal(t, string(BillingRateSourceGroupModel), usageRepo.lastLog.RateSource)
	expected, err := svc.billingService.CalculateCost("claude-sonnet-4", UsageTokens{InputTokens: 10, OutputTokens: 6}, 0.6)
	require.NoError(t, err)
	require.InDelta(t, expected.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
	require.InDelta(t, expected.ActualCost, userRepo.lastAmount, 1e-12)
}

func TestGatewayServiceQuoteUsageCost_UsesModelRate(t *testing.T) {
	groupID := int64(32)
	svc := newGatewayRecordUsageServiceForTest(&openAIRecordUsageLogRepoStub{inserted: true}, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	useGatewayUserGroupRateRepo(svc, &userGroupModelRateRepoStub{cfg: &UserGroupRateConfig{
		ModelRateMultipliers: map[string]float64{"claude-sonnet-*": 0.3},
	}})
	apiKey := &APIKey{ID: 503, GroupID: &groupID, Group: &Group{
		ID:                   groupID,
		RateMultiplier:       0.8,
		ModelRateMultipliers: []GroupModelRateMultiplier{{Models: []string{"claude-sonnet-*"}, RateMultiplier: 0.6}},
	}}

	got := svc.QuoteUsageCost(context.Background(), &RecordUsageInput{
		Result:  &ForwardResult{RequestID: "quote_model_rate", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 6}, Model: "claude-sonnet-4", Duration: time.Second},
		APIKey:  apiKey,
		User:    &User{ID: 603},
		Account: &Account{ID: 703},
	})
	expected, err := svc.billingService.CalculateCost("claude-sonnet-4", UsageTokens{InputTokens: 10, OutputTokens: 6}, 0.3)
	require.NoError(t, err)
	require.InDelta(t, expected.ActualCost, got, 1e-12)
}
