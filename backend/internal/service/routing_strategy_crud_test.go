package service

import "testing"

// validSaveRoutingStrategyInput 返回一份通过校验所需的最小合法输入，测试按需覆盖 GroupIDs。
func validSaveRoutingStrategyInput() *SaveRoutingStrategyInput {
	return &SaveRoutingStrategyInput{
		Name:       "test-strategy",
		Enabled:    true,
		Platform:   PlatformAnthropic,
		MatchMode:  RoutingMatchModeAll,
		Action:     RoutingActionRestrict,
		AccountIDs: []int64{1},
	}
}

func TestNormalizeAndValidateRoutingStrategy_GroupIDsDedupFilterAndOrder(t *testing.T) {
	input := validSaveRoutingStrategyInput()
	// 保序去重，剔除 0 与负数：期望结果为 [5,3,7]。
	input.GroupIDs = []int64{5, 0, 3, 5, -1, 7, 3}

	st, err := normalizeAndValidateRoutingStrategy(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int64{5, 3, 7}
	if len(st.GroupIDs) != len(want) {
		t.Fatalf("expected GroupIDs %v, got %v", want, st.GroupIDs)
	}
	for i, v := range want {
		if st.GroupIDs[i] != v {
			t.Fatalf("expected GroupIDs %v, got %v", want, st.GroupIDs)
		}
	}
}

func TestNormalizeAndValidateRoutingStrategy_EmptyGroupIDsIsLegalAndGlobal(t *testing.T) {
	input := validSaveRoutingStrategyInput()
	input.GroupIDs = nil

	st, err := normalizeAndValidateRoutingStrategy(input)
	if err != nil {
		t.Fatalf("empty GroupIDs must be legal (global scope), got error: %v", err)
	}
	if len(st.GroupIDs) != 0 {
		t.Fatalf("expected empty GroupIDs, got %v", st.GroupIDs)
	}
	if st.GroupIDs == nil {
		t.Fatal("expected non-nil empty slice for GroupIDs so JSON encodes as [] not null")
	}
}

func TestNormalizeAndValidateRoutingStrategy_AllNonPositiveGroupIDsResultsInEmpty(t *testing.T) {
	input := validSaveRoutingStrategyInput()
	input.GroupIDs = []int64{0, -5, -100}

	st, err := normalizeAndValidateRoutingStrategy(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st.GroupIDs) != 0 {
		t.Fatalf("expected all non-positive ids filtered out, got %v", st.GroupIDs)
	}
}

func TestDedupPositiveInt64(t *testing.T) {
	cases := []struct {
		name string
		in   []int64
		want []int64
	}{
		{name: "nil input returns non-nil empty slice", in: nil, want: []int64{}},
		{name: "empty input returns non-nil empty slice", in: []int64{}, want: []int64{}},
		{name: "filters zero and negative", in: []int64{0, -1, -2}, want: []int64{}},
		{name: "dedups preserving first-seen order", in: []int64{3, 1, 3, 2, 1}, want: []int64{3, 1, 2}},
		{name: "mixed valid and invalid", in: []int64{-1, 5, 0, 5, 6}, want: []int64{5, 6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupPositiveInt64(tc.in)
			if got == nil {
				t.Fatal("dedupPositiveInt64 must never return nil")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("expected %v, got %v", tc.want, got)
				}
			}
		})
	}
}
