// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package snapshotmode

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		raw  string
		want Mode
		ok   bool
	}{
		{"full", Full, true},
		{" FULL ", Full, true},
		{"fs-only", FsOnly, true},
		{"FS-ONLY", FsOnly, true},
		{"filesystem", FsOnly, true},
		{"", "", false},
		{"   ", "", false},
		{"fs_only", "", false},
		{"nope", "", false},
	}
	for _, tc := range cases {
		got, err := Parse(tc.raw)
		if (err == nil) != tc.ok {
			t.Fatalf("Parse(%q) error = %v, want ok=%v", tc.raw, err, tc.ok)
		}
		if got != tc.want {
			t.Fatalf("Parse(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]Mode{
		"":           Full,
		"  ":         Full,
		"full":       Full,
		"fs-only":    FsOnly,
		"filesystem": FsOnly,
		"garbage":    Full, // historical rows can only have been full
	}
	for raw, want := range cases {
		if got := Normalize(raw); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestModePredicates(t *testing.T) {
	if !Full.HasMemory() || Full.IsFsOnly() || !Full.Valid() {
		t.Fatal("full mode predicates are wrong")
	}
	if FsOnly.HasMemory() || !FsOnly.IsFsOnly() || !FsOnly.Valid() {
		t.Fatal("fs-only mode predicates are wrong")
	}
	if Mode("").Valid() {
		t.Fatal("empty mode must be invalid")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		message string
		want    Kind
	}{
		{ErrSnapshotModeInvalid + ": bad", KindClientError},
		{ErrFilesystemOnlyRollback + ": snap-1", KindClientError},
		{ErrFilesystemOnlyFreezeUnsupported + ": no agent", KindPrecondition},
		{ErrFilesystemOnlyRootfsUnsupported + ": no disk", KindPrecondition},
		{ErrFilesystemOnlySharedVolume + ": data", KindPrecondition},
		{ErrSnapshotMemoryMissing + ": no memory", KindPrecondition},
		{ErrFilesystemOnlyNodeUnknown + ": no heartbeat", KindUnavailable},
		// Cubelet wraps the identifier and Master stores the whole message, so
		// classification must not depend on the identifier being the first
		// token.
		{"failed to capture sandbox snapshot: " + ErrFilesystemOnlyFreezeUnsupported + ": no agent", KindPrecondition},
		// Server-side failures give the client nothing to act on.
		{ErrFilesystemOnlyFreezeFailed + ": ioctl", KindOther},
		{ErrFilesystemOnlyThawFailed + ": ioctl", KindOther},
		{"dial tcp 10.0.0.1: timeout", KindOther},
		{"", KindOther},
	}
	for _, tc := range cases {
		if got := Classify(tc.message); got != tc.want {
			t.Fatalf("Classify(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}
