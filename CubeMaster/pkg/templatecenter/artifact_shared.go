// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// This file contains artifact-path and build-marker helpers that CubeMaster
// still needs after the image-building logic moved to CubeTemplateCenter.
//
// CubeMaster's responsibilities that require these helpers:
//   - Validating ext4_path in TC's BUILT callback (remote_build_resume.go)
//   - Cleaning up artifact directories during template deletion (artifact_cleanup.go)
//   - Detecting DB-row vs on-disk drift (artifact_presence.go)
//
// CubeMaster does NOT build ext4 files itself; these helpers only locate and
// inspect the shared artifact store.

const (
	defaultArtifactStoreDir  = "/data/CubeMaster/storage"
	fallbackArtifactStoreDir = "cubemaster-rootfs-artifacts-store"
)

// ArtifactWorkRootDir returns the working directory for in-progress builds.
// Only used by TC during the build; Master does not write here.
func ArtifactWorkRootDir() string {
	if value := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR")); value != "" {
		return value
	}
	return filepath.Join(os.TempDir(), "cubemaster-rootfs-artifacts")
}

// ArtifactStoreRootDir returns the finished-artifact store root.
// Both TC (writes) and Master (validates/cleans) share this directory.
func ArtifactStoreRootDir() string {
	if value := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")); value != "" {
		return value
	}
	return defaultArtifactStoreDir
}

// ArtifactFallbackStoreRootDir returns the fallback store root when the
// primary is unavailable. Used for validation and cleanup.
func ArtifactFallbackStoreRootDir() string {
	return filepath.Join(os.TempDir(), fallbackArtifactStoreDir)
}

// artifactStoreDir returns the per-artifact directory under the primary store root.
func artifactStoreDir(artifactID string) string {
	return filepath.Join(ArtifactStoreRootDir(), artifactID)
}

// ResolveArtifactStoreDir resolves the on-disk directory for artifactID,
// creating parent directories as needed. Falls back to the fallback root if
// the primary is unavailable.
//
// Used by artifact cleanup and presence checks on Master.
func ResolveArtifactStoreDir(ctx context.Context, artifactID string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")); configured != "" {
		dir := filepath.Join(configured, artifactID)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", fmt.Errorf("prepare configured artifact store root %s failed: %w", configured, err)
		}
		return dir, nil
	}
	primaryDir := artifactStoreDir(artifactID)
	if err := os.MkdirAll(filepath.Dir(primaryDir), 0o755); err == nil {
		return primaryDir, nil
	} else {
		fallbackDir := filepath.Join(ArtifactFallbackStoreRootDir(), artifactID)
		if fallbackErr := os.MkdirAll(filepath.Dir(fallbackDir), 0o755); fallbackErr == nil {
			log.G(ctx).Warnf("artifact store root %s is unavailable, fallback to %s: %v", ArtifactStoreRootDir(), ArtifactFallbackStoreRootDir(), err)
			return fallbackDir, nil
		} else {
			return "", fmt.Errorf("prepare artifact store root %s failed: %w; fallback %s failed: %v", ArtifactStoreRootDir(), err, ArtifactFallbackStoreRootDir(), fallbackErr)
		}
	}
}

// ext4FixedOverheadMiB returns the fixed overhead (in MiB) reserved when
// computing ext4 filesystem size. Only used by TC during build; kept here
// for reference in case Master needs to validate sizes.
func ext4FixedOverheadMiB() int64 {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_EXT4_FIXED_OVERHEAD_MIB")); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 256
}

// ext4OverheadPercent returns the percentage overhead reserved when computing
// ext4 filesystem size. Only used by TC during build.
func ext4OverheadPercent() int64 {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_EXT4_OVERHEAD_PERCENT")); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 1 && parsed <= 20 {
			return parsed
		}
	}
	return 10
}

// diskSpaceSafetyMargin returns the multiplier applied to estimated image
// size when checking available disk space. Only used by TC during build.
func diskSpaceSafetyMargin() float64 {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_DISK_SPACE_SAFETY_MARGIN")); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed >= 1.0 {
			return parsed
		}
	}
	return 1.5
}

// loopMountExt4Enabled reports whether loop-mount is enabled for ext4 builds.
// Only used by TC during build.
func loopMountExt4Enabled() bool {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_LOOP_MOUNT_EXT4_ENABLED")); v != "" {
		enabled, err := strconv.ParseBool(v)
		return err == nil && enabled
	}
	return false
}

// ---------------------------------------------------------------------------
// Build marker helpers (shared with TC via the filesystem)
// ---------------------------------------------------------------------------

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

// ArtifactBuildInProgress reports whether dir carries a live build marker,
// along with the marker's age. A marker older than artifactBuildMarkerTTL is
// treated as abandoned and reported as not in progress.
//
// Master uses this during cleanup to avoid deleting a directory TC is still
// writing to.
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

// splitFields splits s on whitespace.
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
// Used in cleanup/presence-check error messages on Master.
func DescribeArtifactBuildMarker(dir string) string {
	inProgress, age := ArtifactBuildInProgress(dir)
	if !inProgress {
		return "no live build marker"
	}
	return fmt.Sprintf("build in progress by pid=%d for %s", artifactBuildMarkerPID(dir), age.Truncate(time.Second))
}

// ---------------------------------------------------------------------------
// Image reference validation (Master validates before forwarding to TC)
// ---------------------------------------------------------------------------

// imageRefAllowedPattern is the strict character whitelist for image
// references. It permits exactly the characters that appear in legitimate
// registry/repository[:tag][@algo:hexdigest] references.
var imageRefAllowedPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

// stripImageTransport peels optional docker:// and http(s):// prefixes from an
// image reference. plainHTTP is true only when the caller explicitly wrote
// http://, which selects a plaintext registry.
//
// Master uses this to validate image references before forwarding to TC.
func stripImageTransport(imageRef string) (raw string, plainHTTP bool) {
	raw = strings.TrimPrefix(imageRef, "docker://")
	switch {
	case strings.HasPrefix(raw, "http://"):
		return strings.TrimPrefix(raw, "http://"), true
	case strings.HasPrefix(raw, "https://"):
		return strings.TrimPrefix(raw, "https://"), false
	default:
		return raw, false
	}
}

// ValidateImageRef guards every external image consumer against argument
// injection and rejects syntactically invalid Docker/OCI references.
//
// Optional docker://, http://, and https:// transports are accepted. http://
// selects a plaintext registry on the native pull path; the other prefixes
// are stripped before the semantic parser runs.
//
// Master validates the reference before forwarding the build request to TC.
func ValidateImageRef(imageRef string) error {
	rawRef, _ := stripImageTransport(imageRef)
	if rawRef == "" {
		return errors.New("empty image reference")
	}
	if strings.HasPrefix(rawRef, "docker://") {
		return fmt.Errorf("invalid image reference: %s", imageRef)
	}
	if strings.HasPrefix(rawRef, "-") || !imageRefAllowedPattern.MatchString(rawRef) {
		return fmt.Errorf("invalid image reference: %s", imageRef)
	}
	if _, err := name.ParseReference(rawRef); err != nil {
		return fmt.Errorf("invalid image reference %q: %w", imageRef, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Image config types (shared with TC via callback payload)
// ---------------------------------------------------------------------------

// DockerImageConfig is a small subset of the Docker image config that we
// persist on the template artifact and later reuse when generating the
// container runtime spec.
//
// TC reports this in the BUILT callback; Master stores it on the artifact row.
type DockerImageConfig struct {
	User         string              `json:"User,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
}

// ---------------------------------------------------------------------------
// Pull progress types (shared with TC via Redis snapshot)
// ---------------------------------------------------------------------------

// PullProgressSink receives periodic progress snapshots during image pull.
//
// TC serializes PullProgress to Redis; Master deserializes it for job
// progress queries. Kept in sync with CubeTemplateCenter/pkg/image/progress.go.
type PullProgressSink interface {
	OnProgress(p PullProgress)
}

// LayerProgress is the per-layer progress snapshot.
type LayerProgress struct {
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	Done       bool   `json:"done"`
	Failed     bool   `json:"failed"`
}

// PullProgress is a point-in-time snapshot of image pull progress.
type PullProgress struct {
	TotalLayers     int             `json:"total_layers"`
	DoneLayers      int             `json:"done_layers"`
	FailedLayers    int             `json:"failed_layers"`
	TotalBytes      int64           `json:"total_bytes"`
	DownloadedBytes int64           `json:"downloaded_bytes"`
	Layers          []LayerProgress `json:"layers,omitempty"`

	// Percent is the completion percentage (0-100), derived from bytes when
	// TotalBytes is known, else from completed layers. Mirrors TC's field.
	Percent float64 `json:"percent,omitempty"`

	// SpeedBPS is computed by the sink (Master side) from consecutive snapshots;
	// not serialized by TC. Kept here so the sink can attach it before caching.
	SpeedBPS int64 `json:"speed_bps,omitempty"`

	// CompletedLayers is an alias for DoneLayers used by flush logic.
	// Kept for backward compatibility with existing cache writes.
	CompletedLayers int `json:"completed_layers,omitempty"`
}

// ---------------------------------------------------------------------------
// Utility functions
// ---------------------------------------------------------------------------

// NormalizeBaseURL normalizes a base URL for artifact download. If the input
// lacks a scheme, http:// is prepended.
//
// Master uses this when constructing download URLs for artifacts.
func NormalizeBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed
	}
	return "http://" + trimmed
}
