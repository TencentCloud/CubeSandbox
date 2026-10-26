// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// artifactBuildMarkerName marks an artifact store directory as being actively
// written by a build.
//
// WHY THIS EXISTS
// ---------------
// The artifact store directory is shared state with no single owner:
//
//	<store>/<artifactID>/rootfs                 the rootfs being extracted
//	<store>/<artifactID>/native-prefetch-*      layer tarballs being downloaded
//	<store>/<artifactID>/<artifactID>.ext4      the finished image
//
// The native exporter puts its prefetch temp dir next to `rootfs` (same fast
// disk as the destination, by design), so ANY `RemoveAll(<store>/<artifactID>)`
// performed while a build is running destroys files that build is still
// reading. Inside one process the per-artifact build lock prevents this, but
// CubeTemplateCenter builds while CubeMaster cleans up, and those are two
// processes sharing one disk: an in-process mutex cannot span them.
//
// That is exactly the failure observed as
//
//	native export failed to open prefetched layer 0:
//	open <store>/<artifactID>/native-prefetch-*/layer-000-*.tar: no such file or directory
//
// A marker file is used rather than a DB advisory lock because a lock would
// have to be held for the whole build (minutes), pinning a DB connection for
// its duration; the marker costs nothing and needs no coordination service.
// The file lives at the directory root, a sibling of `rootfs`, so it never
// ends up inside the ext4 (mkfs only consumes `rootfs`).
const artifactBuildMarkerName = ".cube-artifact-build-in-progress"

// artifactBuildMarkerTTL bounds how long a marker is trusted. A process killed
// mid-build cannot remove its own marker, and an unbounded marker would make
// the directory undeletable forever. The bound matches the longest a template
// build is allowed to run before the reconciler declares it dead, so a stale
// marker is only ever ignored after the build it belonged to is already gone.
const artifactBuildMarkerTTL = 2 * time.Hour

// MarkArtifactBuildInProgress publishes the marker for dir and returns the
// function that removes it. The returned function is always safe to call.
//
// The marker is best-effort by design: if it cannot be written the build still
// proceeds (losing a guard is better than failing a build), so the caller may
// ignore the error beyond logging it.
func MarkArtifactBuildInProgress(dir string) (func(), error) {
	if dir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}, fmt.Errorf("prepare artifact dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, artifactBuildMarkerName)
	// The content is diagnostic only; the mtime is what the TTL check reads.
	payload := fmt.Sprintf("pid=%d started_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		return func() {}, fmt.Errorf("write build marker %s: %w", path, err)
	}
	return func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// Leaving the marker behind only delays cleanup until the TTL
			// expires, so this is not worth failing anything over.
			_ = err
		}
	}, nil
}

// ArtifactBuildInProgress reports whether dir carries a live build marker,
// along with the marker's age. A marker older than artifactBuildMarkerTTL is
// treated as abandoned and reported as not in progress.
func ArtifactBuildInProgress(dir string) (bool, time.Duration) {
	if dir == "" {
		return false, 0
	}
	st, err := os.Stat(filepath.Join(dir, artifactBuildMarkerName))
	if err != nil {
		return false, 0
	}
	age := time.Since(st.ModTime())
	if age > artifactBuildMarkerTTL {
		return false, age
	}
	return true, age
}

// artifactBuildMarkerPID reads the pid recorded in dir's marker, for logs.
// Returns 0 when unavailable.
func artifactBuildMarkerPID(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, artifactBuildMarkerName))
	if err != nil {
		return 0
	}
	var pid int
	for _, field := range splitFields(string(data)) {
		if len(field) > 4 && field[:4] == "pid=" {
			pid, _ = strconv.Atoi(field[4:])
		}
	}
	return pid
}

func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// DescribeArtifactBuildMarker renders the marker state for an error message.
func DescribeArtifactBuildMarker(dir string) string {
	inProgress, age := ArtifactBuildInProgress(dir)
	if !inProgress {
		return "no live build marker"
	}
	return fmt.Sprintf("build in progress by pid=%d for %s", artifactBuildMarkerPID(dir), age.Truncate(time.Second))
}
