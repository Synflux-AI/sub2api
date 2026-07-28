package usagestats

import (
	"encoding/json"
	"testing"
)

func TestUserUsageTrendPointIncludesNotes(t *testing.T) {
	payload, err := json.Marshal(UserUsageTrendPoint{
		UserID: 1,
		Email:  "customer@example.com",
		Notes:  "华东客户",
	})
	if err != nil {
		t.Fatalf("marshal user usage trend point: %v", err)
	}

	if got, want := string(payload), `{"date":"","user_id":1,"email":"customer@example.com","username":"","notes":"华东客户","requests":0,"tokens":0,"cost":0,"actual_cost":0}`; got != want {
		t.Fatalf("trend point json=%s want %s", got, want)
	}
}

func TestIsValidModelSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{name: "requested", source: ModelSourceRequested, want: true},
		{name: "upstream", source: ModelSourceUpstream, want: true},
		{name: "mapping", source: ModelSourceMapping, want: true},
		{name: "invalid", source: "foobar", want: false},
		{name: "empty", source: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsValidModelSource(tc.source); got != tc.want {
				t.Fatalf("IsValidModelSource(%q)=%v want %v", tc.source, got, tc.want)
			}
		})
	}
}

func TestNormalizeModelSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "requested", source: ModelSourceRequested, want: ModelSourceRequested},
		{name: "upstream", source: ModelSourceUpstream, want: ModelSourceUpstream},
		{name: "mapping", source: ModelSourceMapping, want: ModelSourceMapping},
		{name: "invalid falls back", source: "foobar", want: ModelSourceRequested},
		{name: "empty falls back", source: "", want: ModelSourceRequested},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeModelSource(tc.source); got != tc.want {
				t.Fatalf("NormalizeModelSource(%q)=%q want %q", tc.source, got, tc.want)
			}
		})
	}
}
