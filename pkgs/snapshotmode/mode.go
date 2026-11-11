// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package snapshotmode defines the user-visible mode of a snapshot artifact.
//
// A mode describes what a snapshot contains, never how a sandbox behaves after
// it has been started: pause/resume always carry memory and have no mode.
package snapshotmode

import (
	"fmt"
	"strings"
)

// Mode is the canonical snapshot artifact mode.
type Mode string

const (
	// Full snapshots carry rootfs + memory + metadata.
	Full Mode = "full"
	// FsOnly snapshots carry rootfs + metadata only.
	FsOnly Mode = "fs-only"
)

// AliasFilesystem is the E2B 2.55 wire value accepted at API boundaries only.
// It is normalized to FsOnly and must never be written to a response, the
// database, a catalog entry, or a log field that describes a stored artifact.
const AliasFilesystem = "filesystem"

// Parse accepts canonical values and the E2B compatibility alias. An empty
// string is not a mode: callers that treat "missing" as the default must use
// Normalize instead. The returned value is always canonical.
func Parse(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(Full):
		return Full, nil
	case string(FsOnly), AliasFilesystem:
		return FsOnly, nil
	case "":
		return "", fmt.Errorf("snapshot mode is empty")
	default:
		return "", fmt.Errorf("invalid snapshot mode %q", raw)
	}
}

// Normalize maps stored or internal values to a canonical mode. Empty means
// full: the mode column was added after full snapshots already existed. Values
// that were never written by this build also fall back to full, because the
// only historical rows are full rows. Client input must go through Parse so an
// unknown value is rejected instead of silently becoming full.
func Normalize(raw string) Mode {
	if strings.TrimSpace(raw) == "" {
		return Full
	}
	mode, err := Parse(raw)
	if err != nil {
		return Full
	}
	return mode
}

// Valid reports whether m is a canonical value.
func (m Mode) Valid() bool {
	return m == Full || m == FsOnly
}

// IsFsOnly reports whether the artifact omits guest memory.
func (m Mode) IsFsOnly() bool { return m == FsOnly }

// HasMemory reports whether the artifact carries guest memory.
func (m Mode) HasMemory() bool { return m == Full }

func (m Mode) String() string { return string(m) }

// Stable error identifiers shared by CubeMaster and Cubelet. They describe the
// filesystem-only feature, not a mode literal, so the names are intentionally
// independent of the canonical "fs-only" wire value.
const (
	ErrFilesystemOnlyUnsupported       = "snapshot_filesystem_only_unsupported_node"
	ErrFilesystemOnlyNodeUnknown       = "snapshot_filesystem_only_node_capability_unknown"
	ErrFilesystemOnlyNotReady          = "snapshot_filesystem_only_not_ready"
	ErrFilesystemOnlyRollback          = "snapshot_filesystem_only_rollback_unsupported"
	ErrFilesystemOnlyCatalog           = "snapshot_filesystem_only_catalog_missing"
	ErrFilesystemOnlyGuestRuntime      = "snapshot_filesystem_only_guest_runtime_mismatch"
	ErrFilesystemOnlyFreezeUnsupported = "snapshot_filesystem_only_freeze_unsupported"
	ErrFilesystemOnlyFreezeFailed      = "snapshot_filesystem_only_freeze_failed"
	ErrFilesystemOnlyThawFailed        = "snapshot_filesystem_only_thaw_failed"
	ErrFilesystemOnlyRootfsUnsupported = "snapshot_filesystem_only_rootfs_unsupported"
	ErrFilesystemOnlySharedVolume      = "snapshot_filesystem_only_shared_volume_unsupported"
	ErrFilesystemOnlyMultiContainer    = "snapshot_filesystem_only_multi_container_unsupported"
	ErrFilesystemOnlyRestoreFailed     = "snapshot_filesystem_only_restore_failed"
	ErrSnapshotMemoryMissing           = "snapshot_memory_missing"
	ErrSnapshotModeInvalid             = "snapshot_mode_invalid"
	ErrSnapshotModeConflict            = "snapshot_mode_conflict"
)

// Kind classifies a failure that carries one of the stable identifiers above.
type Kind int

const (
	// KindOther means the message carries no stable identifier; callers keep
	// their existing default classification.
	KindOther Kind = iota
	// KindClientError means the request itself is invalid (HTTP 400).
	KindClientError
	// KindPrecondition means the request is valid but the cluster or the
	// artifact cannot satisfy it (HTTP 409).
	KindPrecondition
	// KindUnavailable means the answer depends on cluster state that has not
	// arrived yet; the caller may retry (HTTP 503).
	KindUnavailable
)

// identifierKinds is the single classification table for every stable
// identifier that has a defined HTTP status. Server-side failures
// (freeze_failed / thaw_failed / restore_failed) are deliberately absent:
// they give the client nothing to act on and keep the caller's default.
//
// The table mirrors the error contract, including identifiers for the parts of
// the feature that are not wired yet: unsupported_node, not_ready and
// catalog_missing have no producer in this build, and node_capability_unknown
// only becomes reachable once placement reports it. Keeping them classified
// means the status is already right when those producers land.
var identifierKinds = []struct {
	code string
	kind Kind
}{
	{ErrSnapshotModeInvalid, KindClientError},
	{ErrSnapshotModeConflict, KindClientError},
	{ErrFilesystemOnlyRollback, KindClientError},
	{ErrFilesystemOnlyUnsupported, KindPrecondition},
	{ErrFilesystemOnlyNotReady, KindPrecondition},
	{ErrFilesystemOnlyCatalog, KindPrecondition},
	{ErrFilesystemOnlyGuestRuntime, KindPrecondition},
	{ErrFilesystemOnlyFreezeUnsupported, KindPrecondition},
	{ErrFilesystemOnlyRootfsUnsupported, KindPrecondition},
	{ErrFilesystemOnlySharedVolume, KindPrecondition},
	{ErrFilesystemOnlyMultiContainer, KindPrecondition},
	{ErrSnapshotMemoryMissing, KindPrecondition},
	{ErrFilesystemOnlyNodeUnknown, KindUnavailable},
}

// Classify reports how a failure that carries a stable identifier should be
// surfaced.
//
// Identifiers travel inside larger messages: Cubelet wraps them
// ("failed to capture sandbox snapshot: <id>: ...") and Master stores that text
// in the job's error_message, so the match is by containment. Callers must not
// depend on the identifier being the first token.
func Classify(message string) Kind {
	for _, entry := range identifierKinds {
		if strings.Contains(message, entry.code) {
			return entry.kind
		}
	}
	return KindOther
}
