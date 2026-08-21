//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// issue #171：RPM override 必须按「本次请求实际命中的分组」取，而不是默认组。
//
// 复用 billing_cache_service_rpm_test.go 里的 userRPMCacheStub / rpmOverrideRepoStub /
// newBillingServiceForRPM，只补多分组相关的分支。

// 多分组 Key 的快照带的是 UserGroupRPMOverrides（按分组索引），
// 旧的单值 UserGroupRPMOverride 只对默认组有意义。如果 checkRPM 读了单值字段，
// 命中非默认分组的请求就会套用默认组的限额 —— 这正是本 issue 要修的核心错误。
func TestBillingCacheService_CheckRPM_UsesEffectiveGroupOverrideNotDefaultGroup(t *testing.T) {
	const defaultGroupID, effectiveGroupID = int64(10), int64(20)
	defaultOverride := 1 // 默认组：限 1（很紧）
	cache := &userRPMCacheStub{userGroupCounts: []int{1, 2, 3}}
	repo := &rpmOverrideRepoStub{err: errors.New("不应回退 DB：map 里有这个分组的键")}
	svc := newBillingServiceForRPM(t, cache, repo)

	user := &User{
		ID:       1,
		RPMLimit: 1000,
		// 单值字段仍是默认组的紧限额；map 里给命中组一个宽限额。
		UserGroupRPMOverride:  &defaultOverride,
		UserGroupRPMOverrides: map[int64]int{defaultGroupID: 1, effectiveGroupID: 3},
	}
	group := &Group{ID: effectiveGroupID, RPMLimit: 999}

	// 命中组 override=3：前 3 次都应放行。若错读默认组的 1，第 2 次就会被拒。
	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.EqualValues(t, 0, atomic.LoadInt32(&repo.calls), "map 命中时不得回退 DB 查询")
}

// 键存在且值为 0 是**显式免检**，不是零值。误当成「没配」会让该用户在该分组被
// group.rpm_limit 拦住，与管理员的意图相反。
func TestBillingCacheService_CheckRPM_PerGroupOverrideZeroMeansExempt(t *testing.T) {
	cache := &userRPMCacheStub{userCounts: []int{1, 2, 3}}
	repo := &rpmOverrideRepoStub{err: errors.New("不应回退 DB")}
	svc := newBillingServiceForRPM(t, cache, repo)

	user := &User{ID: 1, RPMLimit: 1000, UserGroupRPMOverrides: map[int64]int{20: 0}}
	group := &Group{ID: 20, RPMLimit: 1} // 分组限 1，但 override=0 应让它免检

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.EqualValues(t, 0, atomic.LoadInt32(&cache.userGroupCalls),
		"override=0 免检：不应走 user-group 计数，也不应落到 group.rpm_limit 分支")
	require.EqualValues(t, 0, atomic.LoadInt32(&repo.calls))
}

// 键**不存在**有两种来源（该分组真的没配 / 快照构建时那次查询失败），
// 两者都必须回退 DB 现查，与改造前 nil 的行为一致。
func TestBillingCacheService_CheckRPM_MissingGroupKeyFallsBackToDB(t *testing.T) {
	dbOverride := 2
	cache := &userRPMCacheStub{userGroupCounts: []int{1, 2, 3}}
	repo := &rpmOverrideRepoStub{override: &dbOverride}
	svc := newBillingServiceForRPM(t, cache, repo)

	// map 非空但缺 group 30 这一键。
	user := &User{ID: 1, RPMLimit: 1000, UserGroupRPMOverrides: map[int64]int{20: 5}}
	group := &Group{ID: 30, RPMLimit: 999}

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.ErrorIs(t, svc.checkRPM(context.Background(), user, group), ErrGroupRPMExceeded,
		"应使用 DB 回退查到的 override=2")
	require.EqualValues(t, 3, atomic.LoadInt32(&repo.calls), "缺键必须每次回退 DB")
}

// 单分组路径（map 未装载）必须与改造前逐字相同：读旧的单值字段，不回退 DB。
func TestBillingCacheService_CheckRPM_NilMapKeepsLegacySingleValueBehaviour(t *testing.T) {
	override := 2
	cache := &userRPMCacheStub{userGroupCounts: []int{1, 2, 3}}
	repo := &rpmOverrideRepoStub{err: errors.New("不应回退 DB：单值字段已给出 override")}
	svc := newBillingServiceForRPM(t, cache, repo)

	user := &User{ID: 1, RPMLimit: 1000, UserGroupRPMOverride: &override} // Overrides 为 nil
	group := &Group{ID: 10, RPMLimit: 999}

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.ErrorIs(t, svc.checkRPM(context.Background(), user, group), ErrGroupRPMExceeded)
	require.EqualValues(t, 0, atomic.LoadInt32(&repo.calls))
}
