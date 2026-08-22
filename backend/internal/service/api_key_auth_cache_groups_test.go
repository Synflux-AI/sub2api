//go:build unit

package service

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件守的是 issue #171 引入的「一个分组结构、三处必须同步」的漂移风险：
//
//	APIKeyAuthGroupSnapshot 字段  ←→  groupToAuthSnapshot / authSnapshotToGroup（双向转换）
//	                             ←→  authGroupProjection（SQL 投影，在 repository 包）
//
// 漏任何一处都**不报错**，只会让对应字段拿到零值，于是热路径上的某道门静默失效。
// 多分组之后风险翻倍：同一个分组当默认组时门生效、当非默认绑定组时门失效。

// fillNonZero 用反射把 v 指向的结构体每个字段都填成非零值，返回填了多少个字段。
// 只处理快照里实际出现的种类；遇到不认识的种类直接失败，避免"悄悄跳过"。
func fillNonZero(t *testing.T, v reflect.Value, path string, seed *int) int {
	t.Helper()
	filled := 0
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
		filled++
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*seed++
		v.SetInt(int64(*seed))
		filled++
	case reflect.Float32, reflect.Float64:
		*seed++
		v.SetFloat(float64(*seed) + 0.5)
		filled++
	case reflect.String:
		*seed++
		v.SetString("v" + string(rune('a'+(*seed%26))))
		filled++
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		filled += fillNonZero(t, v.Elem(), path, seed)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		filled += fillNonZero(t, elem, path, seed)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k := reflect.New(v.Type().Key()).Elem()
		filled += fillNonZero(t, k, path, seed)
		val := reflect.New(v.Type().Elem()).Elem()
		filled += fillNonZero(t, val, path, seed)
		v.SetMapIndex(k, val)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !v.Field(i).CanSet() { // 非导出字段
				continue
			}
			filled += fillNonZero(t, v.Field(i), path+"."+f.Name, seed)
		}
	default:
		t.Fatalf("fillNonZero 不认识 %s 的种类 %s —— 请扩展这个 helper，不要跳过", path, v.Kind())
	}
	return filled
}

// TestGroupAuthSnapshotRoundTripLosesNothing 是三处同步的核心对账：
// 把快照的**每个**字段填成非零值，经 authSnapshotToGroup → groupToAuthSnapshot 往返一圈，
// 结果必须与原值逐字节相等。
//
// 任一方向漏赋一个字段，该字段就会在往返后变回零值，本测试立刻失败。
// 这比逐字段手写断言可靠：新增字段不需要改测试也会被覆盖。
func TestGroupAuthSnapshotRoundTripLosesNothing(t *testing.T) {
	var want APIKeyAuthGroupSnapshot
	seed := 0
	filled := fillNonZero(t, reflect.ValueOf(&want).Elem(), "APIKeyAuthGroupSnapshot", &seed)
	require.Greater(t, filled, 50, "填充数明显偏少，说明 helper 跳过了字段")

	// VideoModelPrices 两个方向都会过 NormalizeVideoModelPrices，它按白名单丢弃
	// 非法分辨率键。随机字符串必然被丢掉，所以这里换成合法值，
	// 否则测的是规范化行为而不是往返保真度。
	want.VideoModelPrices = NormalizeVideoModelPrices(map[string]map[string]float64{
		"grok-imagine-video": {
			VideoBillingResolution480P:  0.08,
			VideoBillingResolution720P:  0.14,
			VideoBillingResolution1080P: 0.22,
		},
	})
	require.NotEmpty(t, want.VideoModelPrices, "合法分辨率不应被规范化丢弃")

	group := authSnapshotToGroup(&want)
	require.NotNil(t, group)
	got := groupToAuthSnapshot(group)
	require.NotNil(t, got)

	require.Equal(t, want, *got,
		"快照 → 分组 → 快照 往返丢字段：groupToAuthSnapshot 或 authSnapshotToGroup 少赋值了。"+
			"新增快照分组字段时必须同时改这两个函数与 repository.authGroupProjection")
}

// TestAuthSnapshotToGroupMarksHydrated 钉住 C7。
//
// Hydrated 是从快照还原的分组唯一能声明「字段已完整装载」的标记。漏掉它会让
// service.IsGroupContextValid 判假 → setGroupContext 静默跳过 → 计费 fallback 到
// 调度分组：无报错、无日志的静默错价。这条断言比它看起来重要得多。
func TestAuthSnapshotToGroupMarksHydrated(t *testing.T) {
	g := authSnapshotToGroup(&APIKeyAuthGroupSnapshot{ID: 7, Platform: PlatformOpenAI, Status: "active"})
	require.NotNil(t, g)
	require.True(t, g.Hydrated, "从快照还原的分组必须 Hydrated=true，否则 setGroupContext 会静默跳过")
	require.True(t, IsGroupContextValid(g), "还原出的分组必须能通过 IsGroupContextValid")
}

func TestAuthSnapshotToGroupNilIn_NilOut(t *testing.T) {
	require.Nil(t, authSnapshotToGroup(nil))
	require.Nil(t, groupToAuthSnapshot(nil))
}

// ---------------------------------------------------------------------------
// authRPMOverrideGroupIDs：决定快照构建时要查哪些 (user, group) override
// ---------------------------------------------------------------------------

func TestAuthRPMOverrideGroupIDs(t *testing.T) {
	t.Run("单分组 Key 只查一个，与改造前查询次数相同", func(t *testing.T) {
		k := &APIKey{
			GroupID:     ptrInt64(11),
			BoundGroups: []*Group{{ID: 11, Platform: PlatformAnthropic}},
		}
		require.Equal(t, []int64{11}, authRPMOverrideGroupIDs(k))
	})

	t.Run("默认组与绑定组去重合并且升序", func(t *testing.T) {
		k := &APIKey{
			GroupID: ptrInt64(30),
			BoundGroups: []*Group{
				{ID: 30, Platform: PlatformOpenAI},
				{ID: 10, Platform: PlatformAnthropic},
				{ID: 20, Platform: PlatformGemini},
			},
		}
		require.Equal(t, []int64{10, 20, 30}, authRPMOverrideGroupIDs(k))
	})

	t.Run("BoundGroups 未装载时仍会查默认组", func(t *testing.T) {
		require.Equal(t, []int64{5}, authRPMOverrideGroupIDs(&APIKey{GroupID: ptrInt64(5)}))
	})

	t.Run("未分组 Key 一次都不查", func(t *testing.T) {
		require.Empty(t, authRPMOverrideGroupIDs(&APIKey{}))
		require.Empty(t, authRPMOverrideGroupIDs(nil))
	})

	t.Run("忽略非法 id 与 nil 元素", func(t *testing.T) {
		k := &APIKey{
			GroupID:     ptrInt64(0),
			BoundGroups: []*Group{nil, {ID: 0}, {ID: 9}},
		}
		require.Equal(t, []int64{9}, authRPMOverrideGroupIDs(k))
	})
}

// ---------------------------------------------------------------------------
// 快照 ↔ APIKey 的绑定集合往返
// ---------------------------------------------------------------------------

func TestSnapshotVersionIsBumpedForGroups(t *testing.T) {
	// 存量 v22 快照没有 Groups 字段，反序列化后 len(Groups)==0，而这在新语义里
	// 表示「未分组 Key」—— 不升版会让多分组 Key 在 L2 TTL 内静默退化成单分组。
	require.GreaterOrEqual(t, apiKeyAuthSnapshotVersion, 23,
		"快照新增了 Groups 与 UserGroupRPMOverrides，版本号必须 >= 23")
}
