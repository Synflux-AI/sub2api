//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://example.invalid/v1/messages", nil)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer original-token")
	return r
}

func TestApplyCustomHeaders_DisabledByDefault(t *testing.T) {
	req := newRequest(t)
	a := &Account{
		CustomHeadersEnabled: false,
		CustomHeaders:        map[string]string{"X-Custom": "v"},
	}
	a.ApplyCustomHeaders(req)

	if got := req.Header.Get("X-Custom"); got != "" {
		t.Fatalf("expected X-Custom not set when disabled; got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer original-token" {
		t.Fatalf("Authorization should be untouched when disabled; got %q", got)
	}
}

func TestApplyCustomHeaders_EnabledMergesAndOverrides(t *testing.T) {
	req := newRequest(t)
	a := &Account{
		CustomHeadersEnabled: true,
		CustomHeaders: map[string]string{
			"X-Custom":      "value-1",
			"Authorization": "Bearer overridden", // 显式覆盖应允许
		},
	}
	a.ApplyCustomHeaders(req)

	if got := req.Header.Get("X-Custom"); got != "value-1" {
		t.Fatalf("X-Custom not merged; got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer overridden" {
		t.Fatalf("Authorization should be overridden when admin opts in; got %q", got)
	}
}

func TestApplyCustomHeaders_SkipsProtectedHeaders(t *testing.T) {
	req := newRequest(t)
	a := &Account{
		CustomHeadersEnabled: true,
		CustomHeaders: map[string]string{
			"Host":              "evil.example.com", // 应被忽略
			"Content-Length":    "999",              // 应被忽略
			"Transfer-Encoding": "chunked",          // 应被忽略
			"Connection":        "close",            // 应被忽略
			"X-OK":              "ok",
		},
	}
	a.ApplyCustomHeaders(req)

	for _, blocked := range []string{"Host", "Content-Length", "Transfer-Encoding", "Connection"} {
		if got := req.Header.Get(blocked); got != "" {
			t.Errorf("protected header %q should be skipped; got %q", blocked, got)
		}
	}
	if got := req.Header.Get("X-OK"); got != "ok" {
		t.Errorf("non-protected header should pass through; got %q", got)
	}
}

func TestApplyCustomHeaders_EmptyKeysSkipped(t *testing.T) {
	req := newRequest(t)
	a := &Account{
		CustomHeadersEnabled: true,
		CustomHeaders: map[string]string{
			"":     "ignored",
			"   ":  "also-ignored",
			"X-OK": "ok",
		},
	}
	a.ApplyCustomHeaders(req)
	if got := req.Header.Get("X-OK"); got != "ok" {
		t.Errorf("expected X-OK pass through; got %q", got)
	}
}

func TestApplyCustomHeaders_NilRequestNoop(t *testing.T) {
	a := &Account{
		CustomHeadersEnabled: true,
		CustomHeaders:        map[string]string{"X-Foo": "bar"},
	}
	// 不应 panic
	a.ApplyCustomHeaders(nil)
}

func TestIsCustomHeadersEnabled_RequiresFlagAndNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		acc  *Account
		want bool
	}{
		{"flag off + map", &Account{CustomHeadersEnabled: false, CustomHeaders: map[string]string{"a": "b"}}, false},
		{"flag on + nil map", &Account{CustomHeadersEnabled: true, CustomHeaders: nil}, false},
		{"flag on + empty map", &Account{CustomHeadersEnabled: true, CustomHeaders: map[string]string{}}, false},
		{"flag on + non-empty", &Account{CustomHeadersEnabled: true, CustomHeaders: map[string]string{"a": "b"}}, true},
		{"nil receiver", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.acc.IsCustomHeadersEnabled(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSanitizeCustomHeaders(t *testing.T) {
	in := map[string]string{
		"  X-A  ": "v1",
		"":        "drop",
		"   ":     "drop",
		"X-B":     "v2",
	}
	got := sanitizeCustomHeaders(in)
	if got == nil {
		t.Fatalf("expected non-nil for non-nil input")
		return
	}
	if got["X-A"] != "v1" || got["X-B"] != "v2" {
		t.Errorf("unexpected map: %v", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("empty key should be dropped")
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d", len(got))
	}

	if sanitizeCustomHeaders(nil) != nil {
		t.Errorf("nil input should yield nil output")
	}
}
