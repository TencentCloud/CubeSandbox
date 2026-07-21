// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
)

func TestPublishExt4PublishesImmutableGenerationPath(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
	srcPath := filepath.Join(t.TempDir(), "artifact.ext4")
	content := []byte("ext4 artifact bytes")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	digest := sha256.Sum256(content)
	shaValue := hex.EncodeToString(digest[:])

	path, size, err := PublishExt4(context.Background(), "rfs-test", 2, shaValue, srcPath)
	if err != nil {
		t.Fatalf("PublishExt4: %v", err)
	}
	wantPath := filepath.Join(storeRoot, "rfs-test", "generations", "2", shaValue+".ext4")
	if path != wantPath {
		t.Fatalf("published path=%q, want %q", path, wantPath)
	}
	if size != int64(len(content)) {
		t.Fatalf("published size=%d, want %d", size, len(content))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile published artifact: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("published content=%q, want %q", got, content)
	}
}

func TestPublishExt4RejectsChangedSourceBytes(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
	srcPath := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := os.WriteFile(srcPath, []byte("changed bytes"), 0o644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	wantDigest := sha256.Sum256([]byte("original bytes"))
	wantSHA := hex.EncodeToString(wantDigest[:])

	if _, _, err := PublishExt4(context.Background(), "rfs-test", 3, wantSHA, srcPath); err == nil {
		t.Fatal("PublishExt4 should reject bytes that no longer match the build SHA")
	}
	generationDir := filepath.Join(storeRoot, "rfs-test", "generations", "3")
	entries, err := os.ReadDir(generationDir)
	if err != nil {
		t.Fatalf("ReadDir generation: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed publish left files behind: %v", entries)
	}
}

func TestPublishExt4HonorsCanceledContext(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
	srcPath := filepath.Join(t.TempDir(), "artifact.ext4")
	content := []byte("ext4 bytes")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	digest := sha256.Sum256(content)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := PublishExt4(ctx, "rfs-test", 4, hex.EncodeToString(digest[:]), srcPath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishExt4 error=%v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(filepath.Join(storeRoot, "rfs-test")); !os.IsNotExist(statErr) {
		t.Fatalf("canceled publish should not create shared files: %v", statErr)
	}
}

func TestRemovePublishedExt4OnlyRemovesRequestedGeneration(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
	srcPath := filepath.Join(t.TempDir(), "artifact.ext4")
	content := []byte("ext4 bytes")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	digest := sha256.Sum256(content)
	shaValue := hex.EncodeToString(digest[:])
	publishedPath, _, err := PublishExt4(context.Background(), "rfs-test", 5, shaValue, srcPath)
	if err != nil {
		t.Fatalf("PublishExt4: %v", err)
	}
	otherGeneration := filepath.Join(storeRoot, "rfs-test", "generations", "6")
	if err := os.MkdirAll(otherGeneration, 0o755); err != nil {
		t.Fatalf("MkdirAll other generation: %v", err)
	}
	otherPath := filepath.Join(otherGeneration, "keep.ext4")
	if err := os.WriteFile(otherPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile other generation: %v", err)
	}

	if err := RemovePublishedExt4(context.Background(), "rfs-test", 5, publishedPath); err != nil {
		t.Fatalf("RemovePublishedExt4: %v", err)
	}
	if _, err := os.Stat(publishedPath); !os.IsNotExist(err) {
		t.Fatalf("published generation should be removed: %v", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("other generation must remain: %v", err)
	}
	if err := RemovePublishedExt4(context.Background(), "rfs-test", 5, otherPath); err == nil {
		t.Fatal("RemovePublishedExt4 should reject a path from another generation")
	}
}

func TestBuildExt4ThenPublishExt4KeepsLocalResultUntilCopied(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "work")
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFuncReturn(loopMountExt4Enabled, false)
	patches.ApplyFuncReturn(estimateImageSizeFromInspect, int64(1024), nil)
	patches.ApplyFuncReturn(checkDiskSpace, nil)
	patches.ApplyFunc(exportImageRootfs, func(ctx context.Context, source *PreparedSource, rootfsDir string) error {
		if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(rootfsDir, "rootfs-file"), []byte("rootfs"), 0o644)
	})
	ext4Content := []byte("completed local ext4 bytes")
	patches.ApplyFunc(createExt4Image, func(ctx context.Context, rootfsDir, ext4Path string) error {
		if _, err := os.Stat(filepath.Join(rootfsDir, "rootfs-file")); err != nil {
			return err
		}
		return os.WriteFile(ext4Path, ext4Content, 0o644)
	})

	result, err := BuildExt4(context.Background(), &PreparedSource{}, BuildOptions{ArtifactID: "rfs-integration", Generation: 9})
	if err != nil {
		t.Fatalf("BuildExt4: %v", err)
	}
	if _, err := os.Stat(result.Ext4Path); err != nil {
		t.Fatalf("local ext4 disappeared before publication: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(result.Ext4Path), "rootfs")); !os.IsNotExist(err) {
		t.Fatalf("mutable rootfs should be removed after build: %v", err)
	}

	publishedPath, publishedSize, err := PublishExt4(context.Background(), "rfs-integration", 9, result.SHA256, result.Ext4Path)
	if err != nil {
		t.Fatalf("PublishExt4: %v", err)
	}
	if publishedSize != result.SizeBytes || publishedSize != int64(len(ext4Content)) {
		t.Fatalf("published size=%d build size=%d want=%d", publishedSize, result.SizeBytes, len(ext4Content))
	}
	wantPath := filepath.Join(storeRoot, "rfs-integration", "generations", "9", result.SHA256+".ext4")
	if publishedPath != wantPath {
		t.Fatalf("published path=%q, want %q", publishedPath, wantPath)
	}
	got, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatalf("ReadFile published ext4: %v", err)
	}
	if string(got) != string(ext4Content) {
		t.Fatalf("published content=%q, want %q", got, ext4Content)
	}
}
