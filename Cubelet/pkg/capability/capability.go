// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package capability defines the shared capability vocabulary: a name and a
// level, with names only ever added, never redefined. It is a leaf package that
// imports nothing from Cubelet.
package capability

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Set is a component's declared capabilities. A missing key means not declared;
// level 0 is a positive statement that the capability is unavailable.
type Set map[string]uint32

// Level at which every capability in this document is currently declared.
const (
	LevelInitial uint32 = 1
)

// ── shim capabilities (`update.*`, `errors.*`) ───────────────────────────────
// These names are shared with CubeShim; changing a literal breaks the contract.

const (
	// CapUpdateSnapshotCapture is `SnapshotCapture` / `SnapshotResume`: one
	// freeze window across the memory and the rootfs capture.
	CapUpdateSnapshotCapture = "update.snapshot_capture"
	// CapUpdateSnapshotCaptureFSOnly is `SnapshotCapture` with
	// `snapshot_mode=fs-only`: the rootfs alone, guest still running.
	CapUpdateSnapshotCaptureFSOnly = "update.snapshot_capture.fs_only"
	// CapUpdateCheckPauseAllowed is the `CheckPauseAllowed` open-exec preflight.
	CapUpdateCheckPauseAllowed = "update.check_pause_allowed"
	// CapUpdateRollbackSnapshot is `RollbackSnapshot`.
	CapUpdateRollbackSnapshot = "update.rollback_snapshot"
	// CapUpdatePauseToSnapshot is `PauseToSnapshot`.
	CapUpdatePauseToSnapshot = "update.pause_to_snapshot"
	// CapErrorsCoded means failures carry a stable `code: message` prefix.
	CapErrorsCoded = "errors.coded"
)

// ── runtime (guest) capabilities ─────────────────────────────────────────────

const (
	// CapGuestFSFreeze is the guest agent's filesystem-freeze answer.
	// 1 = supported, 0 = answered and refused; absent = unknown.
	CapGuestFSFreeze = "guest.fs_freeze"
)

// ── coded error prefixes ─────────────────────────────────────────────────────

const (
	// ErrCodeUnknownAction prefixes a rejection of an unrouted Update action.
	ErrCodeUnknownAction = "shim_error_unknown_action"
	// ErrCodePauseBusy prefixes a Pause (or rollback) refusal caused by open exec sessions.
	ErrCodePauseBusy = "shim_error_pause_busy"
)

// LegacyPauseBusyFragments are the pre-capability wording for a pause refusal.
var LegacyPauseBusyFragments = []string{"pause forbidding", "terminate exec tasks first"}

// LegacyUnknownActionFragment is the pre-capability wording for an unrouted action.
const LegacyUnknownActionFragment = "unknown update ext action: "

// ── sources ─────────────────────────────────────────────────────────────────

// Source records where a resolved capability set came from, in trust order.
type Source string

const (
	// SourceRuntime is the running shim's own bundle file.
	SourceRuntime Source = "runtime"
	// SourceArtifact is the released artifact's declaration.
	SourceArtifact Source = "artifact"
	// SourceLegacyTable is the frozen table of versions released before this mechanism.
	SourceLegacyTable Source = "legacy-table"
	// SourceNone means nothing is known; consumers keep their pre-mechanism behaviour.
	SourceNone Source = "none"
)

// File names published by CubeShim.
const (
	// RuntimeFileName is written by the running shim into its bundle directory.
	RuntimeFileName = "cube-shim-capabilities.json"
	// ArtifactFileName sits in the released component tree, next to version.json.
	ArtifactFileName = "capabilities.json"
)

// Bounds on a document read from disk.
const (
	maxDocumentBytes = 64 << 10
	maxEntries       = 128
	maxNameLength    = 128
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Has reports whether name is declared at level >= min.
func (s Set) Has(name string, min uint32) bool {
	level, ok := s[name]
	return ok && level >= min
}

// KnownUnsupported reports whether name is declared below min.
func (s Set) KnownUnsupported(name string, min uint32) bool {
	level, ok := s[name]
	return ok && level < min
}

// Declared reports whether name appears at all, at any level.
func (s Set) Declared(name string) bool {
	_, ok := s[name]
	return ok
}

// Clone returns an independent copy, or nil for an empty set.
func (s Set) Clone() Set {
	if len(s) == 0 {
		return nil
	}
	out := make(Set, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// Equal reports whether both sets declare the same names at the same levels; nil equals empty.
func (s Set) Equal(other Set) bool {
	if len(s) != len(other) {
		return false
	}
	for k, v := range s {
		if ov, ok := other[k]; !ok || ov != v {
			return false
		}
	}
	return true
}

// Names returns the declared names in sorted order, for logs and tests.
func (s Set) Names() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Validate rejects malformed names and sizes in a set from outside this process.
func Validate(s Set) error {
	if len(s) > maxEntries {
		return fmt.Errorf("capability set has %d entries, limit is %d", len(s), maxEntries)
	}
	for name := range s {
		if name == "" {
			return fmt.Errorf("capability name is empty")
		}
		if len(name) > maxNameLength {
			return fmt.Errorf("capability name %q is longer than %d bytes", name, maxNameLength)
		}
		if !validName.MatchString(name) {
			return fmt.Errorf("capability name %q is not a lower-case dotted path", name)
		}
	}
	return nil
}

// CodedErrorCode returns the leading code: token of a coded error message, or empty.
func CodedErrorCode(message string) string {
	code, _, found := strings.Cut(message, ": ")
	if !found || code == "" {
		return ""
	}
	// A code is a bare identifier; prose before a colon ("failed: ...") is not.
	for _, r := range code {
		if (r < 'a' || r > 'z') && r != '.' && r != '_' {
			return ""
		}
	}
	return code
}
