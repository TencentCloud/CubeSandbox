// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/capability"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

// Where a recorded shim version came from, for logging.
const (
	shimVersionSourceComponentPin = "component-pin"
	shimVersionSourceRunTemplate  = "run-template"
	shimVersionSourceUnknown      = "unknown"
)

// pinnedShimVersion resolves the shim version pinned to a sandbox. It locates the
// artifact tree and keys the legacy table; capabilities come from the shim's declaration.
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

// snapshotPathLogger is the narrow logging surface the path selection needs.
type snapshotPathLogger interface {
	Warnf(format string, args ...any)
}

// logSnapshotPathSelection records why the coordinated path was not selected.
func logSnapshotPathSelection(logger snapshotPathLogger, resolved capability.Resolved) {
	switch resolved.Source {
	case capability.SourceNone:
		// Do not claim the shim lacks the action: nothing is known about it.
		logger.Warnf(
			"nothing is known about the CubeShim backing this sandbox (no bundle declaration, "+
				"no artifact declaration, version %q not resolvable), so it cannot be placed "+
				"against %q; using the legacy best-effort snapshot path",
			resolved.Version, capability.CapUpdateSnapshotCapture,
		)
	default:
		logger.Warnf(
			"CubeShim %s (source=%s) does not declare %q (declared=%v); "+
				"using the legacy best-effort snapshot path",
			strings.TrimSpace(resolved.Version), resolved.Source, capability.CapUpdateSnapshotCapture,
			resolved.Capabilities.Names(),
		)
	}
}

// useCoordinatedSnapshotPath reports whether this capture must use the coordinated shim actions.
// fs-only always does; full snapshots follow the declaration.
func useCoordinatedSnapshotPath(resolved capability.Resolved, mode snapshotmode.Mode) bool {
	if mode.IsFsOnly() {
		return true
	}
	return resolved.Capabilities.Has(capability.CapUpdateSnapshotCapture, 1)
}

// shouldRetryWithLegacy reports whether a failed coordinated attempt should be
// redone on the legacy path: the shim rejected the action before producing an artifact.
func shouldRetryWithLegacy(snapshotErr, rootfsErr error, mode snapshotmode.Mode, resolved capability.Resolved) bool {
	// The legacy path always captures memory, so it can never produce a
	// filesystem-only artifact. Retrying would turn an explicit rejection into
	// a silent full snapshot.
	if mode.IsFsOnly() {
		return false
	}
	return rootfsErr == nil && errors.Is(snapshotErr, errSnapshotShimIncompatible) &&
		shimRejectsCapture(snapshotErr, resolved)
}

// logShimDegradedToLegacy records a wrong declaration for this shim.
func logShimDegradedToLegacy(logger snapshotPathLogger, resolved capability.Resolved) {
	logger.Warnf(
		"shim capabilities (source=%s version=%q) declared %q, but the running shim rejected "+
			"the coordinated action; retrying this snapshot with the legacy best-effort path",
		resolved.Source, resolved.Version, capability.CapUpdateSnapshotCapture,
	)
}
