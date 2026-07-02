//go:build unit

// Package citest holds a temporary, intentionally-failing test used to verify
// that Jenkins reports CI failures back onto the PR. Removed before merge.
package citest

import "testing"

func TestCIGateIntentionalFailure(t *testing.T) {
	t.Fatal("intentional failure to verify Jenkins PR failure visibility")
}
