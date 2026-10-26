// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestS3SnapshotRootUnderCubeletWorkPath(t *testing.T) {
	t.Parallel()
	root := S3SnapshotRoot()
	work := CubeletStorageWorkPath()
	if !strings.HasPrefix(work, "/data/cubelet/") {
		t.Fatalf("work path %q is not under /data/cubelet", work)
	}
	if root != filepath.Join(work, S3SnapshotRootName) {
		t.Fatalf("s3 root=%q", root)
	}
	if strings.Contains(root, "cubetoolbox") {
		t.Fatal("s3 snapshots must not live under cubetoolbox")
	}
}

func TestCubeletStorageWorkPathStripsPluginDir(t *testing.T) {
	t.Parallel()
	pluginDir := fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.StorageID)
	got := stripStoragePluginDataDir("/data/cubelet/storage/" + pluginDir)
	if got != "/data/cubelet/storage" {
		t.Fatalf("got %q", got)
	}
	if got := stripStoragePluginDataDir("/data/cubelet/storage"); got != "/data/cubelet/storage" {
		t.Fatalf("plain data_path=%q", got)
	}
}

func TestSnapshotKindRoots(t *testing.T) {
	t.Parallel()
	work := CubeletStorageWorkPath()
	if SnapshotKindRoot("xfs", SnapshotKindNormal) != filepath.Join(work, "xfs", SnapshotKindNormal) {
		t.Fatalf("xfs snapshots=%q", SnapshotKindRoot("xfs", SnapshotKindNormal))
	}
	if SnapshotKindRoot("s3", SnapshotKindPause) != filepath.Join(work, "s3", SnapshotKindPause) {
		t.Fatalf("s3 pause=%q", SnapshotKindRoot("s3", SnapshotKindPause))
	}
	if XFSObjectsDir() != filepath.Join(work, "xfs", SnapshotObjectsDir) {
		t.Fatalf("xfs objects=%q", XFSObjectsDir())
	}
}

func TestEnsureXFSSnapshotPackageMetadataOnly(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "snap-1")
	if err := EnsureSnapshotPackage("xfs", home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, SnapshotMetadataDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, SnapshotMemoryDir)); err == nil {
		t.Fatal("xfs package must not create memory/")
	}
}

func TestEnsureShimSpecDirLink(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "snap-1")
	if err := EnsureSnapshotPackage("xfs", home); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, SnapshotMetadataDir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureShimSpecDirLink(home, "2C2000M"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, "2C2000M", "metadata.json"))
	if err != nil {
		t.Fatalf("shim path: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("metadata via link=%q", got)
	}
	if err := EnsureShimSpecDirLink(home, "2C2000M"); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
}

func TestEnsureS3SnapshotLayout(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "snap-1")
	if err := EnsureS3SnapshotLayout(home); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{S3SnapshotMemoryDir, S3SnapshotDiskDir, S3SnapshotMetadataDir} {
		info, err := os.Stat(filepath.Join(home, d))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", d)
		}
	}
}
