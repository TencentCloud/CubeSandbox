// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The build marker is the only thing standing between a running build and a
// cleanup in ANOTHER process deleting the directory it is writing (the native
// exporter keeps its layer prefetch dir inside the artifact dir). Its whole
// value is in the edge cases, so they are pinned here.

func TestMarkArtifactBuildInProgressRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rfs-round-trip")

	if inProgress, _ := ArtifactBuildInProgress(dir); inProgress {
		t.Fatalf("a directory that does not exist yet must not look like a live build")
	}

	release, err := MarkArtifactBuildInProgress(dir)
	if err != nil {
		t.Fatalf("MarkArtifactBuildInProgress returned an error: %v", err)
	}

	// The directory is created on demand: BuildExt4 marks before it writes.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("marking must create the artifact dir: %v", err)
	}
	inProgress, age := ArtifactBuildInProgress(dir)
	if !inProgress {
		t.Fatalf("a freshly marked directory must report a live build")
	}
	if age < 0 || age > time.Minute {
		t.Fatalf("age of a fresh marker = %v, want a small positive duration", age)
	}

	release()
	if inProgress, _ := ArtifactBuildInProgress(dir); inProgress {
		t.Fatalf("releasing must clear the marker")
	}
	// Cleanup must be able to delete the directory once the build is done.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("directory must be deletable after release: %v", err)
	}
}

func TestMarkArtifactBuildInProgressReleaseIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rfs-double-release")
	release, err := MarkArtifactBuildInProgress(dir)
	if err != nil {
		t.Fatalf("MarkArtifactBuildInProgress returned an error: %v", err)
	}

	// BuildExt4 defers release(); a second call must not panic or error,
	// because a failure path may release explicitly before the defer runs.
	release()
	release()

	if inProgress, _ := ArtifactBuildInProgress(dir); inProgress {
		t.Fatalf("marker must stay cleared after a repeated release")
	}
}

func TestMarkArtifactBuildInProgressSurvivesDirectoryRemoval(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rfs-vanished")
	release, err := MarkArtifactBuildInProgress(dir)
	if err != nil {
		t.Fatalf("MarkArtifactBuildInProgress returned an error: %v", err)
	}

	// Exactly the race the marker exists to prevent, except the other process
	// wins anyway (it may predate this fix, or the marker write may have
	// failed). Release must not panic on a directory that is already gone.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	release()

	if inProgress, _ := ArtifactBuildInProgress(dir); inProgress {
		t.Fatalf("a deleted directory cannot hold a live marker")
	}
}

func TestArtifactBuildInProgressEmptyDirIsNotABuild(t *testing.T) {
	// Guards the call sites: ResolveArtifactStoreDir returns "" together with an
	// error, and treating "" as "a build is running" would make cleanup refuse
	// to ever delete anything.
	if inProgress, age := ArtifactBuildInProgress(""); inProgress || age != 0 {
		t.Fatalf(`ArtifactBuildInProgress("") = (%v, %v), want (false, 0)`, inProgress, age)
	}

	release, err := MarkArtifactBuildInProgress("")
	if err != nil {
		t.Fatalf(`MarkArtifactBuildInProgress("") must be a no-op, got: %v`, err)
	}
	release() // must not panic
}

func TestArtifactBuildInProgressIgnoresDirWithoutMarker(t *testing.T) {
	// An artifact dir left behind by an older build (or by a build that
	// finished) has content but no marker, and must stay deletable.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rfs-x.ext4"), []byte("data"), 0o644); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if inProgress, _ := ArtifactBuildInProgress(dir); inProgress {
		t.Fatalf("a populated directory without a marker must not block cleanup")
	}
}

func TestArtifactBuildInProgressTTLBoundary(t *testing.T) {
	// A process killed mid-build cannot remove its own marker. Without a TTL the
	// directory would be undeletable forever, so the boundary is pinned: just
	// inside the TTL still blocks cleanup, just outside is abandoned.
	tests := []struct {
		name         string
		age          time.Duration
		wantInFlight bool
	}{
		{name: "fresh", age: 0, wantInFlight: true},
		{name: "one-minute-old", age: time.Minute, wantInFlight: true},
		{name: "just-inside-ttl", age: artifactBuildMarkerTTL - time.Minute, wantInFlight: true},
		{name: "just-outside-ttl", age: artifactBuildMarkerTTL + time.Minute, wantInFlight: false},
		{name: "ancient", age: 30 * 24 * time.Hour, wantInFlight: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := MarkArtifactBuildInProgress(dir); err != nil {
				t.Fatalf("precondition: %v", err)
			}
			// The TTL is read from the marker's mtime, so ageing it is enough
			// and the test stays instant.
			markerPath := filepath.Join(dir, artifactBuildMarkerName)
			backdated := time.Now().Add(-tc.age)
			if err := os.Chtimes(markerPath, backdated, backdated); err != nil {
				t.Fatalf("precondition: %v", err)
			}

			inProgress, age := ArtifactBuildInProgress(dir)
			if inProgress != tc.wantInFlight {
				t.Fatalf("ArtifactBuildInProgress(age=%v) = %v, want %v", tc.age, inProgress, tc.wantInFlight)
			}
			// The age is reported even when the marker is judged abandoned, so
			// the cleanup log can say how stale it was.
			if !inProgress && tc.age > artifactBuildMarkerTTL && age < artifactBuildMarkerTTL {
				t.Fatalf("an abandoned marker must still report its age, got %v", age)
			}
		})
	}
}

func TestArtifactBuildMarkerPIDRecordsOwner(t *testing.T) {
	// The pid is what turns "cleanup deferred" into an actionable log line.
	dir := t.TempDir()
	if _, err := MarkArtifactBuildInProgress(dir); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if got, want := artifactBuildMarkerPID(dir), os.Getpid(); got != want {
		t.Fatalf("artifactBuildMarkerPID() = %d, want %d", got, want)
	}
}

func TestArtifactBuildMarkerPIDToleratesGarbage(t *testing.T) {
	// The marker is written by another process and lives on disk, so it is
	// untrusted input: a truncated or hand-edited file must not break the
	// cleanup decision, which only consults the mtime.
	for _, content := range []string{
		"",
		"pid=",
		"pid=notanumber",
		"garbage without fields",
		"started_at=2026-08-19T00:00:00Z",
		strings.Repeat("x", 4096),
	} {
		t.Run(fmt.Sprintf("%.20q", content), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, artifactBuildMarkerName)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("precondition: %v", err)
			}
			if got := artifactBuildMarkerPID(dir); got != 0 {
				t.Fatalf("artifactBuildMarkerPID(%q) = %d, want 0", content, got)
			}
			// The marker must still be honoured: a corrupt body says nothing
			// about whether a build is running.
			if inProgress, _ := ArtifactBuildInProgress(dir); !inProgress {
				t.Fatalf("a corrupt marker body must still block cleanup")
			}
		})
	}
}

func TestArtifactBuildMarkerPIDMissingFile(t *testing.T) {
	if got := artifactBuildMarkerPID(t.TempDir()); got != 0 {
		t.Fatalf("artifactBuildMarkerPID() on an unmarked dir = %d, want 0", got)
	}
}

func TestDescribeArtifactBuildMarker(t *testing.T) {
	dir := t.TempDir()
	if got := DescribeArtifactBuildMarker(dir); got != "no live build marker" {
		t.Fatalf("DescribeArtifactBuildMarker() on an unmarked dir = %q", got)
	}

	if _, err := MarkArtifactBuildInProgress(dir); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	got := DescribeArtifactBuildMarker(dir)
	// The description ends up in an error returned to the GC and in a warning
	// log, so it must name the owner.
	if !strings.Contains(got, "build in progress") {
		t.Fatalf("DescribeArtifactBuildMarker() = %q, want it to mention a build in progress", got)
	}
	if !strings.Contains(got, fmt.Sprintf("pid=%d", os.Getpid())) {
		t.Fatalf("DescribeArtifactBuildMarker() = %q, want it to name pid %d", got, os.Getpid())
	}
}

func TestMarkArtifactBuildInProgressRejectsUncreatableDir(t *testing.T) {
	// A path whose parent is a regular file cannot be turned into a directory.
	// Marking must report the error rather than panic, and the returned release
	// must stay safe to call — BuildExt4 defers it unconditionally and treats
	// the error as a warning, because losing the guard is better than failing
	// a build.
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	release, err := MarkArtifactBuildInProgress(filepath.Join(blocker, "rfs-1"))
	if err == nil {
		t.Fatalf("expected an error when the artifact dir cannot be created")
	}
	release() // must not panic
}

func TestArtifactBuildMarkerLivesOutsideRootfs(t *testing.T) {
	// mkfs consumes <dir>/rootfs. The marker is a sibling of it, so it can
	// never be baked into the produced ext4 image.
	dir := t.TempDir()
	if _, err := MarkArtifactBuildInProgress(dir); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	entries, err := os.ReadDir(rootfs)
	if err != nil {
		t.Fatalf("read rootfs: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rootfs must stay empty, found %d entry/entries", len(entries))
	}
}

func TestMarkArtifactBuildInProgressConcurrentSameDir(t *testing.T) {
	// Two builds of the same fingerprint can race on the marker (the keyed
	// mutex is per-process, and CubeMaster and CubeTemplateCenter are two
	// processes). Marking must be last-writer-wins rather than an error, and
	// the directory must still be reported as busy while any of them runs.
	dir := t.TempDir()
	const workers = 8

	var wg sync.WaitGroup
	releases := make([]func(), workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			releases[i], errs[i] = MarkArtifactBuildInProgress(dir)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed to mark: %v", i, err)
		}
	}
	if inProgress, _ := ArtifactBuildInProgress(dir); !inProgress {
		t.Fatalf("directory must report a live build while workers hold the marker")
	}

	// Releases are also racy; none may panic.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			releases[i]()
		}(i)
	}
	wg.Wait()

	if inProgress, _ := ArtifactBuildInProgress(dir); inProgress {
		t.Fatalf("marker must be cleared once every worker released it")
	}
}
