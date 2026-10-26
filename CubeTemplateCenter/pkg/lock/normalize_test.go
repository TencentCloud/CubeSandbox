// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lock

import (
	"strings"
	"testing"
)

func TestNormalizeLockNameShortPassesThrough(t *testing.T) {
	for _, name := range []string{"", "tc_reconcile_v1", strings.Repeat("a", maxMySQLLockNameLen)} {
		if got := normalizeLockName(name); got != name {
			t.Errorf("normalizeLockName(%q) = %q, want unchanged", name, got)
		}
	}
}

// TestNormalizeLockNameLongFitsMySQLLimit reproduces the exact failure this
// guards against: Error 4163 (42000): User-level lock name '...' should not
// exceed 64 characters, hit by WithBuildLock("tc_build_" + <64-char sha256
// fingerprint>) = 73 bytes.
func TestNormalizeLockNameLongFitsMySQLLimit(t *testing.T) {
	fingerprint := "78ec90fb47a8d622250ecc2392469ea6409b11880a066884ddc9c5f9bd72ec7d"
	long := "tc_build_" + fingerprint
	if len(long) <= maxMySQLLockNameLen {
		t.Fatalf("test fixture too short to exercise truncation: %d bytes", len(long))
	}
	got := normalizeLockName(long)
	if len(got) > maxMySQLLockNameLen {
		t.Fatalf("normalizeLockName(%q) = %q, len=%d exceeds MySQL's %d-char GET_LOCK limit",
			long, got, len(got), maxMySQLLockNameLen)
	}
}

func TestNormalizeLockNameDeterministic(t *testing.T) {
	long := "tc_build_" + strings.Repeat("f", 64)
	a := normalizeLockName(long)
	b := normalizeLockName(long)
	if a != b {
		t.Fatalf("normalizeLockName not deterministic: %q != %q", a, b)
	}
}

// TestNormalizeLockNameDistinguishesDifferentInputs ensures the shortened
// name still depends on the full original name, not just the (identical)
// truncated prefix, so acquire/release for two different long fingerprints
// sharing a common prefix do not collapse onto the same lock.
func TestNormalizeLockNameDistinguishesDifferentInputs(t *testing.T) {
	prefix := "tc_build_" + strings.Repeat("a", 55) // identical prefix for both
	nameA := prefix + "1111111111"
	nameB := prefix + "2222222222"
	if got := normalizeLockName(nameA); got == normalizeLockName(nameB) {
		t.Fatalf("normalizeLockName collapsed two distinct long names onto %q", got)
	}
}

func TestNormalizeLockNameBuildLockNameExample(t *testing.T) {
	// Mirrors WithBuildLock's construction so a regression there is caught
	// here too.
	fingerprint := strings.Repeat("b", 64)
	lockName := "tc_build_" + fingerprint
	got := normalizeLockName(lockName)
	if len(got) > maxMySQLLockNameLen {
		t.Fatalf("WithBuildLock-style name too long after normalization: %d chars", len(got))
	}
	if !strings.HasPrefix(got, "tc_build_") {
		t.Errorf("normalized name %q lost its human-readable prefix", got)
	}
}
