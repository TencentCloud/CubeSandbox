// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func TestResolveRequestStorageBackend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", cow.BackendXFS, false},
		{"xfs", cow.BackendXFS, false},
		{"s3", cow.BackendS3, false},
		{"S3", cow.BackendS3, false},
		{"cos", "", true},
	}
	for _, tc := range cases {
		got, err := resolveRequestStorageBackend(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("resolveRequestStorageBackend(%q) err=nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("resolveRequestStorageBackend(%q) unexpected err: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("resolveRequestStorageBackend(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStorageBackendFromAnnotations(t *testing.T) {
	t.Parallel()
	got, err := storageBackendFromAnnotations(nil)
	if err != nil || got != cow.BackendXFS {
		t.Fatalf("nil annotations: got=%q err=%v", got, err)
	}
	got, err = storageBackendFromAnnotations(map[string]string{})
	if err != nil || got != cow.BackendXFS {
		t.Fatalf("empty annotations: got=%q err=%v", got, err)
	}
	got, err = storageBackendFromAnnotations(map[string]string{
		constants.MasterAnnotationStorageBackend: "s3",
	})
	if err != nil || got != cow.BackendS3 {
		t.Fatalf("s3 annotation: got=%q err=%v", got, err)
	}
	_, err = storageBackendFromAnnotations(map[string]string{
		constants.MasterAnnotationStorageBackend: "cos",
	})
	if err == nil {
		t.Fatal("expected error for unsupported backend")
	}
}

func TestSnapshotDirForRequest(t *testing.T) {
	t.Parallel()
	xfsRoot := storage.SnapshotKindRoot(cow.BackendXFS, storage.SnapshotKindNormal)
	s3Root := storage.SnapshotKindRoot(cow.BackendS3, storage.SnapshotKindNormal)
	if got := snapshotDirForRequest(cow.BackendXFS, ""); got != xfsRoot {
		t.Fatalf("xfs default dir=%q want %q", got, xfsRoot)
	}
	if got := snapshotDirForRequest(cow.BackendXFS, "/custom/xfs"); got != xfsRoot {
		t.Fatalf("xfs must ignore paths outside work tree, got %q", got)
	}
	if got := snapshotDirForRequest(cow.BackendS3, ""); got != s3Root {
		t.Fatalf("s3 default dir=%q, want %q", got, s3Root)
	}
	if got := snapshotDirForRequest(cow.BackendS3, constants.DefaultSnapshotDir); got != s3Root {
		t.Fatalf("s3 must not use cubetoolbox snapshot dir, got %q", got)
	}
}

func TestPrepareSnapshotWorkLayoutS3UsesKindRoot(t *testing.T) {
	t.Parallel()
	layout, err := prepareSnapshotWorkLayout(cow.BackendS3, storage.SnapshotKindNormal, "snap-1", "", "2C2000M")
	if err != nil {
		t.Fatal(err)
	}
	root := storage.SnapshotKindRoot(cow.BackendS3, storage.SnapshotKindNormal)
	if !strings.HasPrefix(root, "/data/cubelet/") {
		t.Fatalf("s3 kind root %q is not under cubelet work path", root)
	}
	if layout.Home != filepath.Join(root, "snap-1") {
		t.Fatalf("s3 home=%q", layout.Home)
	}
	if layout.MetaDir != filepath.Join(layout.Home, "metadata") {
		t.Fatalf("s3 meta=%q", layout.MetaDir)
	}
	if layout.MemoryDir != filepath.Join(layout.Home, "memory") {
		t.Fatalf("s3 memory=%q", layout.MemoryDir)
	}
	if layout.TmpHome != layout.Home || layout.MetaWork != layout.MetaDir {
		t.Fatalf("s3 must write in place, tmp=%q work=%q home=%q meta=%q", layout.TmpHome, layout.MetaWork, layout.Home, layout.MetaDir)
	}
	if layout.MemoryWork != layout.MemoryDir {
		t.Fatalf("s3 memory work must be final dir")
	}
	if layout.usesTmpRename() {
		t.Fatal("s3 must not use tmp+rename")
	}
	if strings.Contains(layout.Home, "cubebox") {
		t.Fatal("s3 layout must not use xfs cubebox nesting")
	}
}

func TestPrepareSnapshotWorkLayoutXFSUsesKindRoot(t *testing.T) {
	t.Parallel()
	layout, err := prepareSnapshotWorkLayout(cow.BackendXFS, storage.SnapshotKindNormal, "snap-1", "", "2C2000M")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(storage.SnapshotKindRoot(cow.BackendXFS, storage.SnapshotKindNormal), "snap-1")
	if layout.Home != want {
		t.Fatalf("xfs home=%q want %q", layout.Home, want)
	}
	if layout.MetaDir != filepath.Join(layout.Home, "metadata") {
		t.Fatalf("xfs meta=%q", layout.MetaDir)
	}
	if layout.MemoryDir != layout.MetaDir {
		t.Fatalf("xfs memory should share metadata, memory=%q", layout.MemoryDir)
	}
	if layout.TmpHome != layout.Home+".tmp" || !layout.usesTmpRename() {
		t.Fatalf("xfs must keep tmp+rename, tmp=%q", layout.TmpHome)
	}
	if strings.Contains(layout.Home, "cubebox") {
		t.Fatal("xfs layout must not use cubebox nesting")
	}
}

func TestPrepareSnapshotWorkLayoutPauseKind(t *testing.T) {
	t.Parallel()
	layout, err := prepareSnapshotWorkLayout(cow.BackendS3, storage.SnapshotKindPause, "snap-p", "", "2C2000M")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(storage.SnapshotKindRoot(cow.BackendS3, storage.SnapshotKindPause), "snap-p")
	if layout.Home != want {
		t.Fatalf("pause home=%q want %q", layout.Home, want)
	}
	if layout.Kind != storage.SnapshotKindPause {
		t.Fatalf("kind=%q", layout.Kind)
	}
}
