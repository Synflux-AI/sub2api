package admin

import (
	"encoding/json"
	"testing"
)

// TestSaveRoutingStrategyRequest_GroupIDsJSONContract 锁定 API 契约：请求体用 group_ids
// （数组，不是旧的单值 group_id），并原样映射进 service 输入。
func TestSaveRoutingStrategyRequest_GroupIDsJSONContract(t *testing.T) {
	body := []byte(`{
		"name": "multi-group",
		"platform": "anthropic",
		"group_ids": [5, 7, 9],
		"match_mode": "all",
		"action": "restrict",
		"account_ids": [1]
	}`)

	var req saveRoutingStrategyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	want := []int64{5, 7, 9}
	if len(req.GroupIDs) != len(want) {
		t.Fatalf("expected GroupIDs %v, got %v", want, req.GroupIDs)
	}
	for i, v := range want {
		if req.GroupIDs[i] != v {
			t.Fatalf("expected GroupIDs %v, got %v", want, req.GroupIDs)
		}
	}

	input := req.toInput()
	if len(input.GroupIDs) != len(want) {
		t.Fatalf("expected mapped input.GroupIDs %v, got %v", want, input.GroupIDs)
	}
	for i, v := range want {
		if input.GroupIDs[i] != v {
			t.Fatalf("expected mapped input.GroupIDs %v, got %v", want, input.GroupIDs)
		}
	}
}

// TestSaveRoutingStrategyRequest_EmptyGroupIDsIsGlobal 验证空数组（全局生效）能正常反序列化，
// 且旧字段 group_id 不再是契约的一部分（发送它不会填充任何字段）。
func TestSaveRoutingStrategyRequest_EmptyGroupIDsIsGlobal(t *testing.T) {
	body := []byte(`{
		"name": "global",
		"platform": "anthropic",
		"group_ids": [],
		"group_id": 42,
		"match_mode": "all",
		"action": "restrict",
		"account_ids": [1]
	}`)

	var req saveRoutingStrategyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if len(req.GroupIDs) != 0 {
		t.Fatalf("expected empty GroupIDs, got %v", req.GroupIDs)
	}

	// 响应序列化不应再出现 group_id：结构体已不含该字段，序列化后 JSON 里也不会有它。
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(out, &asMap); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if _, ok := asMap["group_id"]; ok {
		t.Fatal("serialized request must not contain legacy group_id key")
	}
	if _, ok := asMap["group_ids"]; !ok {
		t.Fatal("serialized request must contain group_ids key")
	}
}
