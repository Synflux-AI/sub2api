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
	// Rates 只在改前改后的专属倍率都读到时才有，读失败不记，免得把读失败记成清空。
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

	if input.GroupRates != nil && oldRates != nil && after.GroupRates != nil {
		for _, id := range slices.Sorted(maps.Keys(input.GroupRates)) {
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
	}
	mark("group_rates", len(changes.Rates) > 0, nil)
	return changes
}
