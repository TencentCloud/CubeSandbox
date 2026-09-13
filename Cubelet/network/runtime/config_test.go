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

// TestConfigEffectiveMTU_LiveLink exercises the netlink branch on a Linux
// runner. Skipped on non-Linux because the production code path is gated on
// netlink availability. The build host for CubeSandbox CI is always Linux.
func TestConfigEffectiveMTU_LiveLink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("netlink-backed MTU lookup requires Linux")
	}
	cfg := Config{MTUInterface: "lo"}
	got := cfg.EffectiveMTU()
	if got <= 0 {
		t.Fatalf("EffectiveMTU() should reflect the live lo MTU, got %d", got)
	}
	if got == DefaultGuestMTU {
		t.Fatalf("EffectiveMTU() fell back to default; live lo lookup should have produced a real value")
	}
}
