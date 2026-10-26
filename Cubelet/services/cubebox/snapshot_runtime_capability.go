// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"

	"golang.org/x/mod/semver"
)

// minCoordinatedSnapshotShimVersion is the first CubeShim release that exposes
// the SnapshotCapture / SnapshotResume actions, which let Cubelet hold a single
// freeze window across the memory and the rootfs capture.
//
// The boundary must be the release candidate, not the final release: 0.7.2-rc2
// sorts *below* 0.7.2 in semantic versioning, and v0.7.2-rc2 is the default
// image tag across the tree. Raising this to a final "v0.7.2" would silently
// send the currently shipped default back to the legacy best-effort path.
//
// The leading "v" is required, not decorative: golang.org/x/mod/semver only
// accepts the canonical v-prefixed form.
const minCoordinatedSnapshotShimVersion = "v0.7.2-rc2"

// Where a resolved shim version came from. Reported so an operator can tell
// "this sandbox is pinned too old" from "this sandbox has no pin at all".
const (
	shimVersionSourceComponentPin = "component-pin"
	shimVersionSourceRunTemplate  = "run-template"
	shimVersionSourceUnknown      = "unknown"
)

// shimSnapshotCapability is what Cubelet knows about the CubeShim backing one
// sandbox: which version it is, and therefore which snapshot actions it has.
type shimSnapshotCapability struct {
	// Version is the resolved, inventory-normalized shim version, or "" when it
	// could not be determined.
	Version string
	// Source records where Version came from.
	Source string
	// Coordinated reports whether Version exposes the coordinated snapshot
	// actions.
	Coordinated bool
}

// resolveShimSnapshotCapability answers "which shim is in play and what can it
// do" for one sandbox, so the path selection and the log line describing it
// cannot disagree. The version itself is resolved by pinnedShimVersion, which
// the cube-runtime path resolver also calls directly.
func resolveShimSnapshotCapability(cb *cubeboxstore.CubeBox) shimSnapshotCapability {
	version, source := pinnedShimVersion(cb)
	return shimSnapshotCapability{
		Version:     version,
		Source:      source,
		Coordinated: coordinatedSnapshotSupported(version),
	}
}

// pinnedShimVersion resolves the shim version pinned to a sandbox, reusing the
// same inventory-normalizing helpers the rest of the package reads versions
// with so every caller agrees on the value.
//
// Only recorded pins are consulted here. A sandbox with no pin deliberately
// takes the legacy path rather than being back-filled from the live toolbox:
// the live tree may point at an upgraded shim even though the running sandbox is
// pinned to older artifacts, and mislabelling it would route a capture through
// actions the running shim does not have.
//
// Note that this is a guarantee about what is *read*, not about what is stored:
// CaptureForCubeBox back-fills missing keys from the live toolbox onto the
// shared CubeBox (and Store.Sync then persists them), so a sandbox that had no
// pin can acquire one after its first snapshot. See the follow-up issue on
// distinguishing recorded pins from back-filled values.
func pinnedShimVersion(cb *cubeboxstore.CubeBox) (version, source string) {
	if cb == nil {
		return "", shimVersionSourceUnknown
	}
	if v := componentVersionFromMap(cb.ComponentVersions, templatetypes.CubeComponentCubeShim); v != "" {
		return v, shimVersionSourceComponentPin
	}
	if v := componentVersionFromMap(versionsFromLocalTemplate(cb.LocalRunTemplate), templatetypes.CubeComponentCubeShim); v != "" {
		return v, shimVersionSourceRunTemplate
	}
	return "", shimVersionSourceUnknown
}

// coordinatedSnapshotSupported reports whether an inventory-normalized shim
// version is at or after minCoordinatedSnapshotShimVersion.
//
// A version that is not semver (a digest, or "") carries no ordering and stays
// on the legacy path. A strictly newer base version is trusted on the base
// comparison alone; only at the boundary's own base does the prerelease decide,
// and an unrecognised scheme ("-vendor", "-s1", "-alpha") has no defined order
// against "-rcN".
func coordinatedSnapshotSupported(version string) bool {
	canonical := canonicalShimSemver(version)
	if canonical == "" {
		return false
	}

	boundaryBase, boundaryRC, ok := splitReleaseCandidate(minCoordinatedSnapshotShimVersion)
	if !ok {
		return false
	}

	switch semver.Compare(baseVersion(canonical), boundaryBase) {
	case 1:
		// A later release train: the prerelease is not consulted.
		return true
	case -1:
		return false
	}

	// Same base version as the boundary, so the prerelease decides.
	prerelease := semver.Prerelease(canonical)
	if prerelease == "" {
		// The final release outranks any prerelease of the same base.
		return true
	}
	rc, ok := releaseCandidateNumber(prerelease)
	if !ok {
		// No defined order against the boundary's "-rcN"; keep the known path.
		return false
	}
	return rc >= boundaryRC
}

// canonicalShimSemver returns the canonical v-prefixed form of version, or ""
// when it is not a semantic version. The gate and the log line share it so they
// cannot disagree about whether a pin can be ordered at all.
//
// A missing "v" is added (release tags are "v0.7.2"-style, a manifest may record
// "0.7.2"), and IsValid is required because semver reports two invalid versions
// as equal, which would make a malformed boundary look supported.
func canonicalShimSemver(version string) string {
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

// releaseCandidateNumber reports the N of a "-rcN" prerelease, and whether the
// prerelease follows the project's rc scheme at all. The number is compared
// numerically on purpose: semver orders prerelease identifiers as text, which
// makes "rc10" sort *below* "rc2" and would drop a two-digit release candidate
// back to the legacy path.
//
// The input is expected in semver's form, including the leading "-". A trailing
// ".<more>" is accepted and ignored, because a longer prerelease sorts above the
// bare rcN.
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
	// Check the digits explicitly: strconv would accept a leading sign, and a
	// hand-written tag like "rc-5" must not be read as a release candidate.
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

// splitReleaseCandidate splits a version into its base and its release-candidate
// number: "v0.7.2-rc2" -> ("v0.7.2", 2).
func splitReleaseCandidate(version string) (base string, rc int, ok bool) {
	version = semver.Canonical(version)
	if version == "" {
		return "", 0, false
	}
	rc, ok = releaseCandidateNumber(semver.Prerelease(version))
	if !ok {
		return "", 0, false
	}
	return baseVersion(version), rc, true
}

// baseVersion strips the prerelease and build metadata: "v0.7.2-rc2" -> "v0.7.2".
func baseVersion(canonical string) string {
	return strings.TrimSuffix(canonical, semver.Prerelease(canonical))
}

// snapshotPathLogger is the narrow logging surface the path selection needs.
type snapshotPathLogger interface {
	Warnf(format string, args ...any)
}

// logSnapshotPathSelection records why the coordinated path was not selected. It
// is a Warn rather than an Info because the sandbox is about to be captured
// without a single freeze window, which callers may need to explain. A version
// that cannot be ordered is not reported as "below" the boundary, which would
// point at an upgrade that cannot change the outcome.
func logSnapshotPathSelection(logger snapshotPathLogger, capability shimSnapshotCapability) {
	if capability.Version == "" {
		// Do not claim the shim lacks the action: nothing is known about it.
		logger.Warnf(
			"CubeShim version is not recorded for this sandbox (source=%s), so it cannot be "+
				"placed against the coordinated snapshot boundary %s; using the legacy "+
				"best-effort snapshot path",
			capability.Source, minCoordinatedSnapshotShimVersion,
		)
		return
	}
	if canonicalShimSemver(capability.Version) == "" {
		logger.Warnf(
			"CubeShim %s (source=%s) is not a comparable semantic version, so it cannot be "+
				"placed against the coordinated snapshot boundary %s; using the legacy "+
				"best-effort snapshot path",
			capability.Version, capability.Source, minCoordinatedSnapshotShimVersion,
		)
		return
	}
	logger.Warnf(
		"CubeShim %s (source=%s) is below the coordinated snapshot boundary %s; "+
			"using the legacy best-effort snapshot path",
		capability.Version, capability.Source, minCoordinatedSnapshotShimVersion,
	)
}

// shouldRetryWithLegacy reports whether a failed coordinated attempt should be
// redone on the legacy path: the shim rejected the action, and no artifact was
// produced before it did.
//
// The retry is only safe because the shim rejects an unknown action before doing
// any work, so the VM was never frozen. The call sites carry the pointer into the
// shim that emits that rejection.
//
// rootfsErr is always nil today: runSnapshotWithRootfs skips the rootfs step once
// the memory step fails. The check is therefore latent coupling rather than a
// live branch, kept and tested so that the invariant stays explicit if that
// helper ever changes.
func shouldRetryWithLegacy(snapshotErr, rootfsErr error) bool {
	return rootfsErr == nil && errors.Is(snapshotErr, errSnapshotShimIncompatible)
}

// logShimDegradedToLegacy records that the version gate selected the coordinated
// path but the running shim rejected the action. It states the two facts that are
// actually known -- the recorded pin is at or above the boundary, and the shim
// rejected the action -- rather than claiming the shim advertises a capability it
// just refused to use. The gate guessed wrong about this shim, so it is worth
// surfacing rather than hiding.
//
// version is non-empty by construction: the retry only runs on the coordinated
// path, which requires a resolved pin.
func logShimDegradedToLegacy(logger snapshotPathLogger, version string) {
	logger.Warnf(
		"recorded CubeShim pin %s is at or above the coordinated snapshot boundary %s, "+
			"but the running shim rejected the coordinated action; retrying this snapshot "+
			"with the legacy best-effort path",
		version, minCoordinatedSnapshotShimVersion,
	)
}
