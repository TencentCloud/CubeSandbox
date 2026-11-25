// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
)

// Boundaries must be canonical, valid semver.
func TestLegacyBoundariesAreCanonical(t *testing.T) {
	require.Equal(t, "v0.7.2-rc2", legacyVersionFloor)
	require.Equal(t, "v0.8.0-rc1", fsOnlyVersionFloor)
	for _, boundary := range []string{legacyVersionFloor, fsOnlyVersionFloor} {
		require.True(t, semver.IsValid(boundary), "%q must be canonical semver", boundary)
		require.True(t, len(boundary) > 0 && boundary[0] == 'v', "%q must be v-prefixed", boundary)
	}
}

// The table covers the released tags in this repository.
func TestLegacyTableCoversReleasedVersions(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    []string
	}{
		// Before the coordinated actions existed at all.
		{"v0.5.0", nil},
		{"v0.6.0", nil},
		{"v0.7.0", nil},
		{"v0.7.1", nil},
		{"v0.7.1-rc2", nil},
		{"v0.7.2-rc1", nil},
		// Coordinated capture, no fs-only yet.
		{"v0.7.2-rc2", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture}},
		{"v0.7.2-rc3", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture}},
		{"v0.7.2", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture}},
		{"v0.7.3-rc1", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture}},
		{"v0.7.3", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture}},
		// fs-only landed in v0.8.0-rc1.
		{"v0.8.0-rc1", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture, CapUpdateSnapshotCaptureFSOnly}},
		{"v0.8.0", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture, CapUpdateSnapshotCaptureFSOnly}},
		{"v0.9.0", []string{CapUpdatePauseToSnapshot, CapUpdateRollbackSnapshot, CapUpdateSnapshotCapture, CapUpdateSnapshotCaptureFSOnly}},
	} {
		t.Run(tc.version, func(t *testing.T) {
			resolved := FromLegacyVersion(tc.version)
			require.Equal(t, SourceLegacyTable, resolved.Source)
			require.ElementsMatch(t, tc.want, resolved.Capabilities.Names())
		})
	}
}

// Pre-mechanism releases must not claim errors.coded or check_pause_allowed.
func TestLegacyTableNeverClaimsPostMechanismCapabilities(t *testing.T) {
	for _, version := range []string{
		"v0.5.0", "v0.6.0", "v0.7.0", "v0.7.1", "v0.7.2-rc2", "v0.7.3", "v0.8.0", "v0.9.0",
	} {
		resolved := FromLegacyVersion(version)
		require.False(t, resolved.Capabilities.Declared(CapErrorsCoded), "%s: errors.coded", version)
		require.False(t, resolved.Capabilities.Declared(CapUpdateCheckPauseAllowed), "%s: check_pause_allowed", version)
		require.Empty(t, resolved.Runtime, "%s: no guest probe result exists for a legacy release", version)
	}
}

// RC numbers are compared numerically, not as text.
func TestLegacyRangeComparesRCNumerically(t *testing.T) {
	t.Run("two_digit_rc_is_at_or_above_a_one_digit_boundary", func(t *testing.T) {
		resolved := FromLegacyVersion("v0.8.0-rc10")
		require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1))
	})
	t.Run("rc10_above_a_rc2_boundary_is_in_range", func(t *testing.T) {
		resolved := FromLegacyVersion("v0.8.0-rc10")
		require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCapture, 1),
			"rc10 of 0.8.0 is above the 0.8.0-rc1 boundary")
	})
	t.Run("rc1_is_below_a_rc2_boundary", func(t *testing.T) {
		resolved := FromLegacyVersion("v0.7.2-rc1")
		require.Empty(t, resolved.Capabilities)
	})
}

// Unorderable versions resolve to SourceNone.
func TestUnorderableVersionsResolveToNone(t *testing.T) {
	for _, version := range []string{
		"",                       // no pin recorded
		"sha256-0123456789ab",    // digest identity, not a version
		"guest-image-260820-1",   // an unrelated tag shape
		"v0.7.2@sha256:deadbeef", // a raw digest-suffixed pin
	} {
		resolved := FromLegacyVersion(version)
		require.Equal(t, SourceNone, resolved.Source, "version %q", version)
		require.True(t, resolved.Empty(), "version %q", version)
	}
}

// Valid but old versions resolve to an explicit empty set.
func TestValidButOldVersionsResolveToAnExplicitEmptySet(t *testing.T) {
	for _, version := range []string{"0.0.0-dev", "v0.7", "v0.7.0"} {
		resolved := FromLegacyVersion(version)
		require.Equal(t, SourceLegacyTable, resolved.Source, "version %q", version)
		require.Empty(t, resolved.Capabilities, "version %q", version)
	}
}

// Prereleases with no defined order against -rcN stay unknown.
func TestAmbiguousPrereleaseSchemesAtTheBoundaryResolveToUnknown(t *testing.T) {
	for _, version := range []string{
		// Internal build numbers — the shape this project actually ships.
		"v0.7.2-1027-1", "0.7.2-1027-1",
		"v0.8.0-1027-1", "0.8.0-1027-1",
		// Vendor / unknown schemes at the boundary's own base version.
		"v0.7.2-vendor", "v0.7.2-s1", "v0.7.2-alpha",
		"v0.8.0-s1", "v0.8.0-vendor", "v0.8.0-alpha",
	} {
		resolved := FromLegacyVersion(version)
		require.Equal(t, SourceNone, resolved.Source,
			"%s sorts nowhere against the boundary and must stay unknown", version)
		require.Empty(t, resolved.Capabilities,
			"%s must not be given either range's capabilities", version)
	}

	// A strictly newer base is decided by the base comparison alone.
	for _, version := range []string{"v0.7.3-vendor", "v0.8.1-s1", "v0.8.1-1027-1"} {
		resolved := FromLegacyVersion(version)
		require.True(t, resolved.Capabilities.Has(CapUpdateSnapshotCapture, 1), version)
	}
	require.True(t, FromLegacyVersion("v0.8.1-s1").Capabilities.Has(CapUpdateSnapshotCaptureFSOnly, 1))

	// A strictly older base version is equally unambiguous.
	require.Empty(t, FromLegacyVersion("v0.7.1-1027-1").Capabilities)
}

// An ambiguous version must not be refused up front for fs-only.
func TestAmbiguousVersionIsNotRefusedUpFrontForFsOnly(t *testing.T) {
	for _, version := range []string{"v0.8.0-1027-1", "0.8.0-1027-1", "v0.8.0-vendor"} {
		resolved := FromLegacyVersion(version)
		require.Equal(t, SourceNone, resolved.Source, version)
		// fsOnlyRefusal keys off SourceNone.
		require.True(t, resolved.Empty(),
			"%s must read as nothing-known so no consumer refuses on it", version)
	}
}

// Build metadata and a missing v are accepted.
func TestCanonicalShimVersionNormalizesPins(t *testing.T) {
	require.Equal(t, "v0.7.2", CanonicalShimVersion("0.7.2"))
	require.Equal(t, "v0.7.2", CanonicalShimVersion("v0.7.2"))
	require.Equal(t, "v0.7.2", CanonicalShimVersion("v0.7.2+build.1"))
	require.Equal(t, "v0.7.2", CanonicalShimVersion("  0.7.2  "))
	require.Empty(t, CanonicalShimVersion(""))
	require.Empty(t, CanonicalShimVersion("sha256-0123456789ab"))
	// A raw digest-suffixed pin is not semver and stays unknown.
	require.Empty(t, CanonicalShimVersion("v0.7.2@sha256:deadbeef"))
}

func TestLegacyEntriesAreOrderedNewestFirst(t *testing.T) {
	require.GreaterOrEqual(t, len(legacyEntries), 2)
	for i := 1; i < len(legacyEntries); i++ {
		newer := CanonicalShimVersion(legacyEntries[i-1].floor)
		older := CanonicalShimVersion(legacyEntries[i].floor)
		require.NotEmpty(t, newer)
		require.NotEmpty(t, older)
		require.Equal(t, 1, semver.Compare(newer, older),
			"entry %d (%s) must be newer than entry %d (%s)", i-1, newer, i, older)
	}
	for _, entry := range legacyEntries {
		require.NotEmpty(t, entry.why, "every range documents the release evidence for it")
		require.NoError(t, Validate(entry.capabilities))
	}
}
