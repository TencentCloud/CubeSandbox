// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// This is the only place in Cubelet allowed to compare shim versions; the table
// is frozen and covers only releases that predate the capability mechanism.

// legacyVersionFloor is the first release with the coordinated snapshot actions.
// Keep it at the rc tag: v0.7.2-rc2 is the default image tag and sorts below v0.7.2.
const legacyVersionFloor = "v0.7.2-rc2"

// fsOnlyVersionFloor is the first release with snapshot_mode=fs-only.
const fsOnlyVersionFloor = "v0.8.0-rc1"

// legacyEntry is one range of released versions and the capabilities it had.
type legacyEntry struct {
	// floor is the first version in the range, inclusive.
	floor string
	// capabilities is the declared set for every version in the range.
	capabilities Set
	// why documents the release evidence for the entry.
	why string
}

// legacyEntries are ordered from newest to oldest.
var legacyEntries = []legacyEntry{
	{
		floor: fsOnlyVersionFloor,
		capabilities: Set{
			CapUpdateSnapshotCapture:       LevelInitial,
			CapUpdateSnapshotCaptureFSOnly: LevelInitial,
			CapUpdateRollbackSnapshot:      LevelInitial,
			CapUpdatePauseToSnapshot:       LevelInitial,
		},
		why: "v0.8.0-rc1 added snapshot_mode=fs-only to the capture payload; " +
			"RollbackSnapshot and PauseToSnapshot predate the capability mechanism",
	},
	{
		floor: legacyVersionFloor,
		capabilities: Set{
			CapUpdateSnapshotCapture:  LevelInitial,
			CapUpdateRollbackSnapshot: LevelInitial,
			CapUpdatePauseToSnapshot:  LevelInitial,
		},
		why: "v0.7.2-rc2 added the coordinated SnapshotCapture / SnapshotResume actions",
	},
}

// FromLegacyVersion maps a pinned shim version to the capability set that release had.
// Unparseable or ambiguously-ordered versions resolve to SourceNone.
func FromLegacyVersion(version string) Resolved {
	canonical := CanonicalShimVersion(version)
	if canonical == "" {
		return Resolved{Source: SourceNone, Version: version}
	}
	for _, entry := range legacyEntries {
		switch compareToBoundary(canonical, entry.floor) {
		case orderAtOrAbove:
			return Resolved{
				Capabilities: entry.capabilities.Clone(),
				Source:       SourceLegacyTable,
				Version:      version,
			}
		case orderAmbiguous:
			// Ambiguous order: stop here; a lower boundary is equally ambiguous.
			return Resolved{Source: SourceNone, Version: version}
		}
	}
	// Provably older than the first release with any of these actions.
	return Resolved{Source: SourceLegacyTable, Version: version, Capabilities: Set{}}
}

// CanonicalShimVersion returns the canonical v-prefixed form of version, or empty when invalid.
func CanonicalShimVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if !semver.IsValid(version) {
		return ""
	}
	// IsValid passed, so Canonical only drops build metadata.
	return semver.Canonical(version)
}

// order says how a version sits relative to a release-candidate boundary.
type order int

const (
	// orderBelow means the version is provably older than the boundary.
	orderBelow order = iota
	// orderAtOrAbove means the version is provably at or newer than the boundary.
	orderAtOrAbove
	// orderAmbiguous means the prerelease has no defined order against the boundary.
	orderAmbiguous
)

// compareToBoundary orders a canonical shim version against an rc boundary.
// Both arguments must be valid semver.
func compareToBoundary(canonical, boundary string) order {
	boundaryBase, boundaryRC, ok := splitReleaseCandidate(boundary)
	if !ok {
		// A malformed boundary has no order against anything.
		return orderAmbiguous
	}

	switch semver.Compare(baseVersion(canonical), boundaryBase) {
	case 1:
		return orderAtOrAbove
	case -1:
		return orderBelow
	}

	// Same base version as the boundary, so the prerelease decides.
	prerelease := semver.Prerelease(canonical)
	if prerelease == "" {
		// The final release outranks any prerelease of the same base.
		return orderAtOrAbove
	}
	rc, ok := releaseCandidateNumber(prerelease)
	if !ok {
		return orderAmbiguous
	}
	if rc >= boundaryRC {
		return orderAtOrAbove
	}
	return orderBelow
}

// releaseCandidateNumber reports the N of a -rcN prerelease and whether the
// prerelease follows the rc scheme. The number is compared numerically.
func releaseCandidateNumber(prerelease string) (int, bool) {
	rest, ok := strings.CutPrefix(prerelease, "-rc")
	if !ok {
		return 0, false
	}
	digits := rest
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		digits = rest[:i]
	}
	if digits == "" {
		return 0, false
	}
	// Check digits explicitly: strconv would accept a leading sign.
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	rc, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return rc, true
}

// splitReleaseCandidate splits a version into its base and rc number.
func splitReleaseCandidate(version string) (base string, rc int, ok bool) {
	canonical := CanonicalShimVersion(version)
	if canonical == "" {
		return "", 0, false
	}
	rc, ok = releaseCandidateNumber(semver.Prerelease(canonical))
	if !ok {
		return "", 0, false
	}
	return baseVersion(canonical), rc, true
}

// baseVersion strips the prerelease and build metadata: "v0.7.2-rc2" -> "v0.7.2".
func baseVersion(canonical string) string {
	return strings.TrimSuffix(canonical, semver.Prerelease(canonical))
}
