package service

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 开启「模型冷却兜底调度」后，「账号+模型」短期冷却只是偏好：有别的号可选时避开
// 冷却中的号，只剩它时照常调度；关闭时保持原行为（本地 503）。
// 回归场景：分组里某模型只有一个中转账号，一个用户的请求级错误把它冷却 45s，
// 整组该模型全部本地 503、一次上游都不打。

const transientFailOpenModel = "gpt-5.4"

type transientFailOpenMode struct {
	name      string
	loadBatch bool
	advanced  bool
}

var transientFailOpenModes = []transientFailOpenMode{
	{name: "legacy", loadBatch: false},
	{name: "legacy_load_batch", loadBatch: true},
	{name: "advanced", advanced: true},
}

func newTransientFailOpenAccount(id int64) Account {
	return Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
}

func newTransientFailOpenService(mode transientFailOpenMode, accounts []Account) *OpenAIGatewayService {
	return newTransientFailOpenServiceWithSwitch(mode, accounts, true)
}

func newTransientFailOpenServiceWithSwitch(mode transientFailOpenMode, accounts []Account, failOpenEnabled bool) *OpenAIGatewayService {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	values := map[string]string{
		SettingKeyOpenAIModelCooldownFailOpenEnabled: strconv.FormatBool(failOpenEnabled),
	}
	if mode.advanced {
		values[openAIAdvancedSchedulerSettingKey] = "true"
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = mode.loadBatch
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		rateLimitService: &RateLimitService{
			settingService: NewSettingService(&openAIAdvancedSchedulerSettingRepoStub{values: values}, &config.Config{}),
		},
	}
	return svc
}

// coolDownAccountModel 让账号在该模型上连续失败两次，进入 10s 短期冷却。
func coolDownAccountModel(t *testing.T, svc *OpenAIGatewayService, account *Account, model string) {
	t.Helper()
	canonical := canonicalOpenAIAccountSchedulingModel(account, model)
	now := time.Now()
	svc.recordOpenAIAccountModelTransientFailure(account, canonical, now)
	decision := svc.recordOpenAIAccountModelTransientFailure(account, canonical, now)
	require.Positive(t, decision.Cooldown)
	require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(account, model))
}

func TestSelectAccountWithScheduler_SingleAccountInModelCooldownStillSelected(t *testing.T) {
	for _, mode := range transientFailOpenModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			groupID := int64(10901)
			only := newTransientFailOpenAccount(39001)
			svc := newTransientFailOpenService(mode, []Account{only})
			require.Equal(t, mode.advanced, svc.isOpenAIAdvancedSchedulerEnabled(ctx))
			coolDownAccountModel(t, svc, &only, transientFailOpenModel)

			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", transientFailOpenModel, nil, OpenAIUpstreamTransportAny, false)

			require.NoError(t, err)
			require.NotNil(t, selection)
			require.NotNil(t, selection.Account)
			require.Equal(t, only.ID, selection.Account.ID)
			// fail-open 只放行本次选号，不清除冷却状态。
			require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(&only, transientFailOpenModel))
		})
	}
}

func TestSelectAccountWithScheduler_ModelCooldownFailOpenDisabledKeepsLocal503(t *testing.T) {
	for _, mode := range transientFailOpenModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			groupID := int64(10905)
			only := newTransientFailOpenAccount(39007)
			svc := newTransientFailOpenServiceWithSwitch(mode, []Account{only}, false)
			require.False(t, svc.isOpenAIModelCooldownFailOpenEnabled(ctx))
			coolDownAccountModel(t, svc, &only, transientFailOpenModel)

			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", transientFailOpenModel, nil, OpenAIUpstreamTransportAny, false)

			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)
		})
	}
}

func TestOpenAIModelCooldownFailOpen_DefaultDisabled(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{
			settingService: NewSettingService(&openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{}}, &config.Config{}),
		},
	}
	require.False(t, svc.isOpenAIModelCooldownFailOpenEnabled(context.Background()))
	require.False(t, (&OpenAIGatewayService{}).isOpenAIModelCooldownFailOpenEnabled(context.Background()))
}

func TestSelectAccountWithScheduler_ModelCooldownStillAvoidedWhenAlternativeExists(t *testing.T) {
	for _, mode := range transientFailOpenModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			groupID := int64(10902)
			cooled := newTransientFailOpenAccount(39002)
			cooled.Priority = 0
			healthy := newTransientFailOpenAccount(39003)
			// 冷却中的号优先级更高，不避开它的话会被选中。
			healthy.Priority = 9
			svc := newTransientFailOpenService(mode, []Account{cooled, healthy})
			coolDownAccountModel(t, svc, &cooled, transientFailOpenModel)

			for range 5 {
				selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", transientFailOpenModel, nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.NotNil(t, selection.Account)
				require.Equal(t, healthy.ID, selection.Account.ID)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			}
		})
	}
}

func TestSelectAccountWithScheduler_ModelCooldownFailOpenRespectsExclusions(t *testing.T) {
	for _, mode := range transientFailOpenModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			groupID := int64(10903)
			only := newTransientFailOpenAccount(39004)
			svc := newTransientFailOpenService(mode, []Account{only})
			coolDownAccountModel(t, svc, &only, transientFailOpenModel)

			// failover 刚在这个号上失败过：第二轮也不能把它再放出来。
			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", transientFailOpenModel, map[int64]struct{}{only.ID: {}}, OpenAIUpstreamTransportAny, false)

			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)
			require.Contains(t, err.Error(), "excluded=1")
		})
	}
}

func TestSelectAccountWithScheduler_ModelCooldownFailOpenKeepsAccountLevelBlock(t *testing.T) {
	for _, mode := range transientFailOpenModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			groupID := int64(10904)
			only := newTransientFailOpenAccount(39005)
			resetAt := time.Now().Add(time.Minute)
			only.RateLimitResetAt = &resetAt
			svc := newTransientFailOpenService(mode, []Account{only})
			svc.BlockAccountScheduling(&only, resetAt, "test_rate_limited")
			coolDownAccountModel(t, svc, &only, transientFailOpenModel)

			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", transientFailOpenModel, nil, OpenAIUpstreamTransportAny, false)

			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)
		})
	}
}

func TestIsOpenAIAccountRequestRuntimeBlocked_BypassSkipsOnlyModelCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTransientFailOpenAccount(39006)
	coolDownAccountModel(t, svc, &account, transientFailOpenModel)
	bypass := withOpenAIModelTransientCooldownBypass(context.Background())

	require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(context.Background(), &account, transientFailOpenModel))
	require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(bypass, &account, transientFailOpenModel))

	// 账号级屏蔽 + 持久化冷却字段仍然生效，bypass 不能绕过。
	resetAt := time.Now().Add(time.Minute)
	account.RateLimitResetAt = &resetAt
	svc.BlockAccountScheduling(&account, resetAt, "test_rate_limited")
	require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(bypass, &account, transientFailOpenModel))
}

func TestOpenAIModelTransient_ActiveBlockCount(t *testing.T) {
	state := newOpenAIAccountModelTransientState(16)
	now := time.Now()
	require.Zero(t, state.activeBlockCount(now))

	state.recordFailure(1, "gpt-5.4", now)
	require.Zero(t, state.activeBlockCount(now), "第一次失败只记数不冷却")

	state.recordFailure(1, "gpt-5.4", now)
	state.recordFailure(2, "gpt-5.4", now)
	state.recordFailure(2, "gpt-5.4", now)
	require.Equal(t, 2, state.activeBlockCount(now))
	require.Zero(t, state.activeBlockCount(now.Add(openAIModelTransientShortCooldown+time.Second)))

	state.recordSuccess(1, "gpt-5.4")
	require.Equal(t, 1, state.activeBlockCount(now))

	var nilState *openAIAccountModelTransientState
	require.Zero(t, nilState.activeBlockCount(now))
}
