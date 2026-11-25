// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The agent table answers for cube-agent releases that predate the artifact
// declaration, so its boundary and the sets it maps to are the only thing deciding
// "can this old agent freeze a filesystem".
func TestLegacyAgentTableCoversReleasedVersions(t *testing.T) {
	for _, tc := range []struct {
		version string
		freeze  bool
	}{
		// Before the freeze RPCs existed at all.
		{"v0.5.0", false},
		{"v0.6.0", false},
		{"v0.7.0", false},
		{"v0.7.1", false},
		{"v0.7.2", false},
		{"v0.7.3-rc1", false},
		{"v0.7.3", false},
		// An internal build number from the same train: orderable, below.
		{"0.7.3-1027-1", false},
		// The freeze RPCs landed in v0.8.0-rc1 together.
		{"v0.8.0-rc1", true},
		{"v0.8.0-rc4", true},
		{"v0.8.0", true},
		{"v0.8.1", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			resolved := FromLegacyAgentVersion(tc.version)
			require.Equal(t, SourceLegacyTable, resolved.Source)
			require.Equal(t, tc.freeze, resolved.Capabilities.Has(CapGuestFSFreeze, 1))
		})
	}
}

// An agent version whose order against the boundary cannot be proven must resolve
// to unknown, not to either side: guessing "cannot freeze" would refuse a
// filesystem-only capture that may work, and guessing "can freeze" would send the
// capture through RPCs the agent may not serve.
func TestLegacyAgentAmbiguousVersionsResolveToUnknown(t *testing.T) {
	for _, version := range []string{
		"v0.8.0-1027-1", "0.8.0-1027-1", // internal build number at the boundary
		"v0.8.0-vendor", "v0.8.0-s1", // vendor suffixes at the boundary
		"", "sha256-0123456789ab", // no order at all
	} {
		resolved := FromLegacyAgentVersion(version)
		require.Equal(t, SourceNone, resolved.Source, version)
		require.True(t, resolved.Empty(),
			"%s must read as nothing-known so no consumer refuses on it", version)
	}
}

// A strictly newer train is decided on the base version alone, so the prerelease is
// irrelevant there.
func TestLegacyAgentLaterBaseVersionIsTrusted(t *testing.T) {
	for _, version := range []string{"v0.8.1-1027-1", "v0.8.1-vendor", "v0.9.0"} {
		resolved := FromLegacyAgentVersion(version)
		require.True(t, resolved.Capabilities.Has(CapGuestFSFreeze, 1), version)
	}
}

// The boundary is pinned to the release that actually introduced the RPCs, so a
// change to it has to be deliberate.
func TestAgentFSFreezeFloorIsPinned(t *testing.T) {
	require.Equal(t, "v0.8.0-rc1", agentFSFreezeFloor)
}
