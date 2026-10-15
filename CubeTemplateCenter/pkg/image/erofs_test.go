// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
)

// TestBuildErofsProducesErofsImage exercises the full BuildErofs pipeline with a
// REAL mkfs.erofs: exportImageRootfs is patched out to materialize a small fake
// rootfs, but createErofsImage / mkfs.erofs / computeFileSHA256 run for real so
// the test proves the command-line shape (output-first, source-dir-second) and
// the artifact store layout actually produce a valid .erofs artifact.
//
// Skipped when mkfs.erofs is not on PATH (e.g. dev machines without erofs-utils).
func TestBuildErofsProducesErofsImage(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skipf("mkfs.erofs not installed: %v", err)
	}

	workRoot := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	const artifactID = "artifact-erofs-e2e"

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(estimateImageSizeFromInspect, func(ctx context.Context, source *PreparedSource) (int64, error) {
		return 1024, nil
	})
	patches.ApplyFunc(checkDiskSpace, func(ctx context.Context, storeDir string, estimatedSizeBytes int64) error {
		return nil
	})
	patches.ApplyFunc(isLocalFastFS, func(path string) bool { return true })
	patches.ApplyFunc(exportImageRootfs, func(ctx context.Context, source *PreparedSource, destRootfsDir string) error {
		// Materialize a minimal but real rootfs tree for mkfs.erofs to pack.
		if err := os.MkdirAll(filepath.Join(destRootfsDir, "bin"), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(destRootfsDir, "etc"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destRootfsDir, "etc", "hello.txt"), []byte("erofs-e2e"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(destRootfsDir, "bin", "hello"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	})

	postCalled := false
	opts := BuildOptions{
		ArtifactID: artifactID,
		PostRootfsExport: func(ctx context.Context, rootfsDir string) error {
			postCalled = true
			if _, err := os.Stat(filepath.Join(rootfsDir, "etc", "hello.txt")); err != nil {
				return err
			}
			return nil
		},
	}

	result, err := BuildErofs(context.Background(), &PreparedSource{LocalRef: "docker.io/library/nginx:latest", ExportMode: ExportModeNative}, opts)
	if err != nil {
		t.Fatalf("BuildErofs failed: %v", err)
	}

	// The published artifact lives under the store root with a .erofs suffix.
	erofsPath := filepath.Join(storeRoot, artifactID, artifactID+".erofs")
	if result.Ext4Path != erofsPath {
		t.Fatalf("result path=%q, want %q", result.Ext4Path, erofsPath)
	}
	if result.SHA256 == "" || len(result.SHA256) != 64 {
		t.Fatalf("expected a 64-char sha256, got %q", result.SHA256)
	}
	if result.SizeBytes <= 0 {
		t.Fatalf("expected positive size, got %d", result.SizeBytes)
	}

	info, err := os.Stat(erofsPath)
	if err != nil {
		t.Fatalf("erofs artifact not published: %v", err)
	}
	if info.Size() != result.SizeBytes {
		t.Fatalf("reported size %d != on-disk size %d", result.SizeBytes, info.Size())
	}

	// The microVM boots this as a virtio-pmem backing file, whose size MUST be
	// a multiple of 2 MiB or the host device manager rejects it
	// ("PmemSizeNotAligned"). The fake rootfs above is tiny so mkfs.erofs's
	// output is not 2 MiB-aligned on its own — this asserts createErofsImage
	// pads it up.
	const twoMiB = int64(2 * 1024 * 1024)
	if result.SizeBytes%twoMiB != 0 {
		t.Fatalf("erofs artifact size %d is not a multiple of 2 MiB (virtio-pmem requires alignment)", result.SizeBytes)
	}

	// Verify it is genuinely an EROFS image (file(1) reads the superblock magic).
	out, err := exec.Command("file", "-b", erofsPath).Output()
	if err != nil {
		t.Logf("file(1) unavailable, skipping magic check: %v", err)
	} else if !strings.Contains(string(out), "EROFS") {
		t.Fatalf("expected EROFS image, file says: %s", strings.TrimSpace(string(out)))
	}

	if !postCalled {
		t.Fatal("PostRootfsExport callback must run before mkfs.erofs packs the rootfs")
	}

	// The exported rootfs must be cleaned up after a successful build (the store
	// dir keeps only the final artifact, not the rootfs tree).
	if _, err := os.Stat(filepath.Join(storeRoot, artifactID, "rootfs")); !os.IsNotExist(err) {
		t.Fatalf("rootfs dir should be removed after successful build, stat err=%v", err)
	}
}
