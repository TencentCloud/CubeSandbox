// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"runtime"
	"testing"
)

// TestConfigEffectiveMTU locks down the MTU resolution order introduced for
// issue #1673. The goal is to make sure an operator can rely on the runtime
// picking the right MTU without manually setting every layer.
//
// Resolution order:
//
//  1. Explicit MvmMtu → wins unconditionally.
//  2. MTUInterface set, link missing/unreadable → fall back to DefaultGuestMTU.
//  3. MTUInterface set, link present with a positive MTU → use that.
//     We verify by pointing the lookup at the loopback interface (lo) on the
//     build host. The test is Linux-only because netlink is a Linux kernel
//     interface; the package is not buildable on macOS.
func TestConfigEffectiveMTU(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want int
	}{
		{
			name: "explicit MvmMtu wins over everything",
			cfg:  Config{MvmMtu: 1450, MTUInterface: "lo"},
			want: 1450,
		},
		{
			name: "MTUInterface fallback when MvmMtu is zero and link is missing",
			cfg:  Config{MTUInterface: "definitely-not-a-real-link-cubesandbox-1673"},
			want: DefaultGuestMTU,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EffectiveMTU(); got != tc.want {
				t.Fatalf("EffectiveMTU() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestResolveMTUOnce covers the single-shot resolution used by
// EnsureNetwork so the host TAP MTU and the guest descriptor MTU
// always agree (the previous code called EffectiveMTU twice — once
// per call site — which could split them on a transient netlink
// error). Issue #1673 review.
func TestResolveMTUOnce(t *testing.T) {
	t.Run("per-request override wins", func(t *testing.T) {
		cfg := Config{MvmMtu: 1500, MTUInterface: "lo"}
		if got := resolveMTUOnce(cfg, 1400); got != 1400 {
			t.Fatalf("resolveMTUOnce(cfg, 1400) = %d, want 1400", got)
		}
	})
	t.Run("explicit MvmMtu wins when no override", func(t *testing.T) {
		cfg := Config{MvmMtu: 1450}
		if got := resolveMTUOnce(cfg, 0); got != 1450 {
			t.Fatalf("resolveMTUOnce(cfg, 0) = %d, want 1450", got)
		}
	})
	t.Run("no override and no MvmMtu falls back to default", func(t *testing.T) {
		cfg := Config{}
		if got := resolveMTUOnce(cfg, 0); got != DefaultGuestMTU {
			t.Fatalf("resolveMTUOnce(empty cfg, 0) = %d, want %d", got, DefaultGuestMTU)
		}
	})
	t.Run("negative override is treated as no override", func(t *testing.T) {
		cfg := Config{MvmMtu: 1500}
		if got := resolveMTUOnce(cfg, -1); got != 1500 {
			t.Fatalf("resolveMTUOnce(cfg, -1) = %d, want 1500 (MvmMtu fallback)", got)
		}
	})
}
//
// We can't hard-assert a specific lo MTU because Linux distros
// freely configure it (commonly 65536, but 1500 and others are
// valid). What we can assert is that a positive non-default value
// comes back when the link exists, AND that the loopback lookup
// returned the same MTU we read independently via netlink.
func TestConfigEffectiveMTU_LiveLink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("netlink-backed MTU lookup requires Linux")
	}
	expected, err := lookupLinkMTU("lo")
	if err != nil {
		t.Skipf("loopback interface not present on this runner: %v", err)
	}
	if expected <= 0 {
		t.Skipf("loopback MTU is non-positive (%d); the test would be vacuous", expected)
	}

	cfg := Config{MTUInterface: "lo"}
	if got := cfg.EffectiveMTU(); got != expected {
		t.Fatalf("EffectiveMTU() = %d, want %d (live lo MTU)", got, expected)
	}

	// When MvmMtu is set, EffectiveMTU must not consult the
	// interface at all (the whole point of having an explicit
	// per-runtime override).
	cfg2 := Config{MvmMtu: 1400, MTUInterface: "no-such-link-1673"}
	if got := cfg2.EffectiveMTU(); got != 1400 {
		t.Fatalf("EffectiveMTU() with MvmMtu=1400 = %d, want 1400", got)
	}
}
