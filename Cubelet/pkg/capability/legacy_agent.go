// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package capability

// Frozen table of cube-agent releases that predate the capability mechanism, and
// the only place in Cubelet that compares agent versions. It never grows: every
// later release publishes its own declaration.

// agentFSFreezeFloor is the first cube-agent release that serves both FreezeRootfs
// and ThawRootfs. One boundary covers the pair because they landed together, and a
// build with only one of them would wedge a frozen filesystem.
const agentFSFreezeFloor = "v0.8.0-rc1"

// FromLegacyAgentVersion maps a recorded cube-agent version to what that release
// could do. A version whose order against the boundary is not provable resolves to
// SourceNone, which keeps the caller on its pre-mechanism behaviour.
func FromLegacyAgentVersion(version string) Resolved {
	canonical := CanonicalVersion(version)
	if canonical == "" {
		return Resolved{Source: SourceNone, Version: version}
	}
	switch compareToBoundary(canonical, agentFSFreezeFloor) {
	case orderAtOrAbove:
		return Resolved{
			Capabilities: Set{CapGuestFSFreeze: LevelInitial},
			Source:       SourceLegacyTable,
			Version:      version,
		}
	case orderAmbiguous:
		return Resolved{Source: SourceNone, Version: version}
	default:
		return Resolved{Source: SourceLegacyTable, Version: version, Capabilities: Set{}}
	}
}
