// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func exportImageRootfs(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
	if source == nil {
		return errors.New("resolved source image is nil")
	}
	if source.ExportMode != ExportModeNative {
		if err := ValidateImageRef(source.LocalRef); err != nil {
			return err
		}
	}

	if source.ExportMode == ExportModeNative {
		if err := os.RemoveAll(destRootfsDir); err != nil {
			return err
		}
		if err := os.MkdirAll(destRootfsDir, 0o755); err != nil {
			return err
		}
		return StreamRegistryToDir(ctx, source, destRootfsDir)
	}

	// Use the strategy chosen at prepare time rather than re-detecting, so the
	// prepare and export phases never diverge.
	if source.ExportMode == ExportModeDockerless {
		return dockerlessExportImageRootfs(ctx, source, destRootfsDir)
	}
	return dockerExportImageRootfs(ctx, source, destRootfsDir)
}

// dockerlessExportImageRootfs exports the source image's root filesystem
// without a docker daemon. The flow is:
//
//  1. `skopeo copy docker://<ref> oci:<workDir>/image` pulls the image into a
//     local OCI layout (honoring the optional skopeo auth file).
//  2. `umoci unpack --rootless` materializes that layout into an OCI bundle
//     under <workDir>/bundle, whose `rootfs` subdir holds the extracted files.
//  3. The bundle's rootfs is moved into place at destRootfsDir via os.Rename.
//
// The scratch workDir is created under destRootfsDir's parent (rather than
// $TMPDIR) so the final os.Rename stays on the same filesystem and cannot fail
// with EXDEV, which would otherwise force a slow full copy across devices.
func dockerlessExportImageRootfs(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
	if err := os.MkdirAll(filepath.Dir(destRootfsDir), 0o755); err != nil {
		return err
	}
	workDir, err := os.MkdirTemp(filepath.Dir(destRootfsDir), ".dockerless-rootfs-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	ociDir := filepath.Join(workDir, "image")
	bundleDir := filepath.Join(workDir, "bundle")

	sourceRef := skopeoDockerImageRef(source.LocalRef)
	ociImageRef := "oci:" + ociLayoutImageRef(ociDir, source.LocalRef)
	skopeoArgs := []string{"copy"}
	if source.SkopeoAuthFile != "" {
		skopeoArgs = append(skopeoArgs, "--authfile", source.SkopeoAuthFile)
	}
	skopeoArgs = append(skopeoArgs, sourceRef, ociImageRef)
	if err := skopeoCopy(ctx, skopeoArgs, source.OnPullProgress, source.CompressedSizeBytes); err != nil {
		return fmt.Errorf("skopeo copy %s failed: %w", source.LocalRef, err)
	}

	umociImageRef := ociLayoutImageRef(ociDir, source.LocalRef)
	// Only pass --rootless when we are NOT running as root. --rootless makes
	// umoci squash every file's owner to the unpacking user (and skip device
	// nodes), so when cubemaster runs as root it rewrites every image uid/gid
	// to 0. That silently mutates the image's ownership and breaks any image
	// relying on non-root-owned files with restrictive modes — e.g. a
	// python/browser image whose /home/user is uid 1000 mode 0700 becomes
	// root:root 0700, so the in-sandbox "user" account can no longer access
	// its own home: envd exec fails (fork/exec /bin/sh EACCES) and chromium
	// can't write its profile (crash-loop, CDP port never binds). Running as
	// root without --rootless preserves the image's original uid/gid exactly.
	umociArgs := []string{"unpack"}
	if os.Geteuid() != 0 {
		umociArgs = append(umociArgs, "--rootless")
	}
	umociArgs = append(umociArgs, "--image", umociImageRef, bundleDir)
	if err := runCommand(ctx, "", "umoci", umociArgs...); err != nil {
		return fmt.Errorf("umoci unpack %s failed: %w", source.LocalRef, err)
	}
	// The OCI layout is no longer needed once unpacked; drop it early to reduce
	// peak disk usage on the artifact filesystem.
	_ = os.RemoveAll(ociDir) // NOCC:Path Traversal()

	unpackedRootfsDir := filepath.Join(bundleDir, "rootfs")
	if err := os.RemoveAll(destRootfsDir); err != nil { // NOCC:Path Traversal()
		return err
	}
	if err := os.Rename(unpackedRootfsDir, destRootfsDir); err != nil { // NOCC:Path Traversal()
		return fmt.Errorf("move unpacked rootfs failed: %w", err)
	}
	return nil
}

// hasDockerlessRootfsExportTools reports whether both skopeo and umoci are
// available on PATH. When they are, the build prefers the daemonless export
// path (skopeo+umoci) over docker. This auto-detection means installing both
// tools on a cubemaster node silently switches the source-image handling away
// from the docker daemon; EnsureArtifactBuildPreflight reflects the resulting
// command requirements.
func hasDockerlessRootfsExportTools() bool {
	if _, err := executableLookPath("skopeo"); err != nil {
		return false
	}
	if _, err := executableLookPath("umoci"); err != nil {
		return false
	}
	return true
}

func dockerExportImageRootfs(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
	containerIDBytes, err := dockerOutput(ctx, "", "create", "--", source.LocalRef)
	if err != nil {
		return fmt.Errorf("docker create %s failed: %w", source.LocalRef, err)
	}
	containerID := strings.TrimSpace(string(containerIDBytes))
	// Use context.Background() so that cleanup runs even after request ctx is cancelled.
	cleanupCtx := context.Background()
	defer func() {
		_ = dockerRun(cleanupCtx, "", "rm", "-f", "--", containerID)
	}()

	if err := os.RemoveAll(destRootfsDir); err != nil { // NOCC:Path Traversal()
		return err
	}
	if err := os.MkdirAll(destRootfsDir, 0o755); err != nil {
		return err
	}

	return pipeExportToDir(ctx, containerID, destRootfsDir)
}
