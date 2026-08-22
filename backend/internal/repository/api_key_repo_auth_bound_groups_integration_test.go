//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 这是 issue #171 整条链路上**最承重**的一环，单独立文件是为了让它显眼。
//
// 认证走的是 GetByKeyForAuth（显式列投影 + eager-load）。选组的第一步是
// `len(apiKey.BoundGroups) <= 1` 就走快速路径。所以一旦这个查询不返回绑定集合，
// 或者 entity→service 的映射漏了它：
//
//   - 所有多分组 Key 都会被判成单分组，选组彻底失效；
//   - 全部请求按默认组计费，usage_log.group_id 全是默认组；
//   - **不报错、不打日志、测不出来**（其它测试都在 service 层直接构造 APIKey，
//     绕过了这个查询）。
//
// 所以这里必须从真实 DB 出发验证：写入绑定 → GetByKeyForAuth → 绑定集合完整回来，
// 且每个分组的计费字段都有值（投影没漏列）。
func TestGetByKeyForAuthLoadsBoundGroupsWithBillingFields(t *testing.T) {
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAPIKeyRepositoryWithSQL(client, tx)
	ctx := t.Context()

	user, err := client.User.Create().
		SetEmail("auth-bound-groups@test.com").
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// 两个不同平台、不同倍率的分组，并各带一个「漏列就会静默失效」的字段。
	anthropic, err := client.Group.Create().
		SetName("auth-bound-anthropic").
		SetPlatform(service.PlatformAnthropic).
		SetStatus(service.StatusActive).
		SetRateMultiplier(1.0).
		SetProfitControlEnabled(true).
		SetRpmLimit(11).
		Save(ctx)
	if err != nil {
		t.Fatalf("create anthropic group: %v", err)
	}
	openai, err := client.Group.Create().
		SetName("auth-bound-openai").
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetRateMultiplier(1.2).
		SetProfitControlEnabled(true).
		SetRpmLimit(22).
		Save(ctx)
	if err != nil {
		t.Fatalf("create openai group: %v", err)
	}

	anthropicSvc := groupEntityToService(anthropic)
	openaiSvc := groupEntityToService(openai)

	key := &service.APIKey{
		UserID:      user.ID,
		Key:         "sk-auth-bound-groups",
		Name:        "auth-bound-groups",
		Status:      service.StatusActive,
		GroupID:     &anthropic.ID,
		BoundGroups: []*service.Group{anthropicSvc, openaiSvc},
	}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	got, err := repo.GetByKeyForAuth(ctx, key.Key)
	if err != nil {
		t.Fatalf("GetByKeyForAuth: %v", err)
	}

	if len(got.BoundGroups) != 2 {
		t.Fatalf("认证查询必须返回全部 2 个绑定分组，得到 %d —— "+
			"漏了这个 eager-load，所有多分组 Key 都会被判成单分组，选组静默失效",
			len(got.BoundGroups))
	}

	// 顺序必须是 (platform, id) 升序：anthropic < openai。选组依赖它稳定。
	if got.BoundGroups[0].ID != anthropic.ID || got.BoundGroups[1].ID != openai.ID {
		t.Fatalf("绑定集合顺序应为 (platform, id) 升序，得到 %d,%d",
			got.BoundGroups[0].ID, got.BoundGroups[1].ID)
	}

	// 默认组语义不变。
	if got.GroupID == nil || *got.GroupID != anthropic.ID {
		t.Fatalf("默认组指针必须仍指向 anthropic")
	}
	if got.Group == nil || got.Group.ID != anthropic.ID {
		t.Fatalf("默认组对象必须仍是 anthropic")
	}

	// 每个绑定分组的计费字段都必须有值 —— 绑定组与默认组共用同一份列投影
	// （authGroupProjection），漏列不会报错，只会让对应的门拿到零值静默失效。
	byID := map[int64]*service.Group{}
	for _, g := range got.BoundGroups {
		byID[g.ID] = g
	}
	if g := byID[openai.ID]; g == nil {
		t.Fatalf("缺 openai 绑定分组")
	} else {
		if g.RateMultiplier != 1.2 {
			t.Fatalf("非默认绑定分组的 rate_multiplier 必须原样返回（按命中分组计费的前提），得到 %v",
				g.RateMultiplier)
		}
		if !g.ProfitControlEnabled {
			t.Fatalf("非默认绑定分组的 profit_control_enabled 丢了 —— " +
				"这正是「漏列让利润控制门静默失效」的形态")
		}
		if g.RPMLimit != 22 {
			t.Fatalf("非默认绑定分组的 rpm_limit 必须原样返回，得到 %d", g.RPMLimit)
		}
		if g.Platform != service.PlatformOpenAI {
			t.Fatalf("非默认绑定分组的 platform 必须原样返回（选组按它匹配），得到 %q", g.Platform)
		}
	}
}
