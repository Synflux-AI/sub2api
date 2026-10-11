package service

import (
	"maps"
	"slices"
)

// UserChanges 是一次 UpdateUser 实际改了什么。请求体只有改后的值（allowed_groups 还是整张名单），
// 审计要靠它记下改前值，才看得出这次加了、去了哪个分组，倍率从多少改到多少。
type UserChanges struct {
	// Fields 是实际变了的字段，用请求里的 JSON 名；密码无法比较，提交了就算。
	Fields []string
	// Old 是 role、status、concurrency、rpm_limit 中变了的那几项的改前值。
	Old           map[string]any
	GroupsAdded   []int64
	GroupsRemoved []int64
	// Rates 只在改前改后的专属倍率都读到时才有；读失败时 Fields 仍含 group_rates、Rates 为空，
	// 免得把读失败记成清空，也不把真实的修改记成没变。
	Rates []UserGroupRateChange
}

// UserGroupRateChange 的 Old／New 为 nil 表示该分组用默认倍率。
type UserGroupRateChange struct {
	GroupID int64
	Old     *float64
	New     *float64
}

func diffUserChanges(before, after *User, input *UpdateUserInput, oldRates map[int64]float64) UserChanges {
	changes := UserChanges{Old: map[string]any{}}
	mark := func(field string, changed bool, old any) {
		if !changed {
			return
		}
		changes.Fields = append(changes.Fields, field)
		if old != nil {
			changes.Old[field] = old
		}
	}
	mark("email", before.Email != after.Email, nil)
	mark("password", input.Password != "", nil)
	mark("username", before.Username != after.Username, nil)
	mark("notes", before.Notes != after.Notes, nil)
	mark("role", before.Role != after.Role, before.Role)
	mark("status", before.Status != after.Status, before.Status)
	mark("concurrency", before.Concurrency != after.Concurrency, before.Concurrency)
	mark("rpm_limit", before.RPMLimit != after.RPMLimit, before.RPMLimit)
	mark("restrict_public_groups", before.RestrictPublicGroups != after.RestrictPublicGroups, nil)

	for _, id := range after.AllowedGroups {
		if !slices.Contains(before.AllowedGroups, id) {
			changes.GroupsAdded = append(changes.GroupsAdded, id)
		}
	}
	for _, id := range before.AllowedGroups {
		if !slices.Contains(after.AllowedGroups, id) {
			changes.GroupsRemoved = append(changes.GroupsRemoved, id)
		}
	}
	mark("allowed_groups", len(changes.GroupsAdded)+len(changes.GroupsRemoved) > 0, nil)

	// 比对改前改后的全部分组：提交空表会清掉所有专属倍率，只看请求里的键会漏掉
	if input.GroupRates != nil && oldRates != nil && after.GroupRates != nil {
		ids := slices.Sorted(maps.Keys(oldRates))
		for id := range after.GroupRates {
			if _, ok := oldRates[id]; !ok {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			old, hadOld := oldRates[id]
			now, hasNew := after.GroupRates[id]
			if hadOld == hasNew && old == now {
				continue
			}
			change := UserGroupRateChange{GroupID: id}
			if hadOld {
				change.Old = &old
			}
			if hasNew {
				change.New = &now
			}
			changes.Rates = append(changes.Rates, change)
		}
		mark("group_rates", len(changes.Rates) > 0, nil)
	} else {
		// 读不到改前或改后的倍率，就说不清变没变：照样标出提交了倍率，Rates 留空，读的一方回退去看请求体
		mark("group_rates", input.GroupRates != nil, nil)
	}
	return changes
}
