// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package patchoverlay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func TestWithCubeUseRefPath(t *testing.T) {
	var cfg SnapshotterConfig
	if err := WithCubeUseRefPath(&cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.useCubeRefPath {
		t.Fatalf("useCubeRefPath should be enabled")
	}
}

func TestNewCubeRefPath(t *testing.T) {
	root := t.TempDir()
	ctx := namespaces.WithNamespace(context.Background(), "ns")
	sn := &snapshotter{root: root, useCubeRefPath: true}

	t.Run("no label", func(t *testing.T) {
		p, err := sn.makeCubeRefPathDir(ctx, snapshots.Info{Labels: map[string]string{}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p != "" {
			t.Fatalf("expected empty path, got %s", p)
		}
	})

	t.Run("create ref path", func(t *testing.T) {
		info := snapshots.Info{Labels: map[string]string{
			constants.AnnotationSnapshotRef: constants.PrefixSha256 + "real",
		}}
		p, err := sn.makeCubeRefPathDir(ctx, info)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(p)); err != nil {
			t.Fatalf("expected path to exist: %v", err)
		}
	})

	t.Run("cube ref path disabled", func(t *testing.T) {
		disabled := &snapshotter{root: root}
		p, err := disabled.makeCubeRefPathDir(ctx, snapshots.Info{Labels: map[string]string{}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p != "" {
			t.Fatalf("expected empty path when disabled")
		}
	})
}

func TestGetCubeRefPath(t *testing.T) {
	root := t.TempDir()
	ctx := namespaces.WithNamespace(context.Background(), "ns")
	info := snapshots.Info{Labels: map[string]string{
		constants.AnnotationSnapshotRef: constants.PrefixSha256 + "abc",
	}}

	disabled := &snapshotter{root: root}
	if p, err := disabled.genCubeRefPath(ctx, info); err != nil || p != "" {
		t.Fatalf("expected empty path when disabled")
	}

	enabled := &snapshotter{root: root, useCubeRefPath: true}
	if p, err := enabled.genCubeRefPath(ctx, snapshots.Info{Labels: map[string]string{}}); err != nil || p != "" {
		t.Fatalf("expected empty path without label")
	}

	if _, err := enabled.genCubeRefPath(context.Background(), info); err == nil {
		t.Fatalf("expected namespace error")
	}

	p, err := enabled.genCubeRefPath(ctx, info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != filepath.Join(root, refDirName, "ns", "abc") {
		t.Fatalf("unexpected path %s", p)
	}
}

func TestGetCubeUpperAndWorkPath(t *testing.T) {
	root := t.TempDir()
	ctx := namespaces.WithNamespace(context.Background(), "ns")
	id := "123"

	disabled := &snapshotter{root: root}
	if got := disabled.getValidCubeUpperPath(ctx, id, snapshots.Info{}); got != disabled.upperPath(id) {
		t.Fatalf("expected default upper path")
	}
	if got := disabled.getCubeWorkPath(ctx, id, snapshots.Info{}); got != disabled.workPath(id) {
		t.Fatalf("expected default work path")
	}

	infoExternal := snapshots.Info{Labels: map[string]string{
		constants.AnnotationSnapshotterExternalPath: "/external/path",
	}}
	enabled := &snapshotter{root: root, useCubeRefPath: true}
	if got := enabled.getValidCubeUpperPath(ctx, id, infoExternal); got != "/external/path" {
		t.Fatalf("expected external path, got %s", got)
	}

	infoRef := snapshots.Info{Labels: map[string]string{
		constants.AnnotationSnapshotRef: constants.PrefixSha256 + "abc",
	}}
	if got := enabled.getValidCubeUpperPath(context.Background(), id, infoRef); got != enabled.upperPath(id) {
		t.Fatalf("expected fallback upper path on error")
	}
	if got := enabled.getCubeWorkPath(context.Background(), id, infoRef); got != enabled.workPath(id) {
		t.Fatalf("expected fallback work path on error")
	}

	infoEmpty := snapshots.Info{Labels: map[string]string{}}
	if got := enabled.getValidCubeUpperPath(ctx, id, infoEmpty); got != enabled.upperPath(id) {
		t.Fatalf("expected fallback upper path on empty ref")
	}
	if got := enabled.getCubeWorkPath(ctx, id, infoEmpty); got != enabled.workPath(id) {
		t.Fatalf("expected fallback work path on empty ref")
	}

	if got := enabled.getValidCubeUpperPath(ctx, id, infoRef); !strings.HasSuffix(got, "abc/fs") {
		t.Fatalf("unexpected upper path %s", got)
	}
	if got := enabled.getCubeWorkPath(ctx, id, infoRef); !strings.HasSuffix(got, "abc/work") {
		t.Fatalf("unexpected work path %s", got)
	}
}

func TestTryCommitWithRefPath(t *testing.T) {
	root := t.TempDir()
	ctx := namespaces.WithNamespace(context.Background(), "ns")
	id := "123"

	disabled := &snapshotter{root: root}
	if opts, err := disabled.tryCommitWithRefPath(ctx, snapshots.Info{Labels: map[string]string{}}, "name", id); err != nil || len(opts) != 0 {
		t.Fatalf("disabled snapshotter should no-op")
	}

	enabled := &snapshotter{root: root, useCubeRefPath: true}
	if opts, err := enabled.tryCommitWithRefPath(ctx, snapshots.Info{Labels: map[string]string{
		constants.AnnotationSnapshotRef: "preset",
	}}, "name", id); err != nil || len(opts) != 0 {
		t.Fatalf("preset ref should skip rename")
	}

	if opts, err := enabled.tryCommitWithRefPath(ctx, snapshots.Info{Labels: map[string]string{}}, "short", id); err != nil || len(opts) != 0 {
		t.Fatalf("invalid name should skip")
	}

	infoNilLabels := snapshots.Info{}
	oldSnapshotPathNilLabels := filepath.Dir(enabled.upperPath(id + "-nil-labels"))
	if err := os.MkdirAll(oldSnapshotPathNilLabels, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := enabled.tryCommitWithRefPath(ctx, infoNilLabels, "ns/meta/"+constants.PrefixSha256+"nil-labels", id+"-nil-labels"); err != nil {
		t.Fatalf("nil labels should not fail: %v", err)
	}

	info := snapshots.Info{Labels: map[string]string{}}
	oldSnapshotPath := filepath.Dir(enabled.upperPath(id))
	if err := os.MkdirAll(oldSnapshotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldSnapshotPath, "dummy"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "ns/meta/" + constants.PrefixSha256 + "digest"
	opts, err := enabled.tryCommitWithRefPath(ctx, info, name, id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("expected label option to be returned")
	}
	refPath := filepath.Join(root, refDirName, "ns", "digest")
	if _, err := os.Stat(refPath); err != nil {
		t.Fatalf("expected ref path to exist: %v", err)
	}
	if _, err := os.Stat(oldSnapshotPath); err == nil {
		t.Fatalf("expected old snapshot path removed")
	}
	if info.Labels[constants.AnnotationSnapshotRef] != constants.PrefixSha256+"digest" {
		t.Fatalf("label not updated")
	}
}

func TestGetCleanupRefDirectories(t *testing.T) {
	root := t.TempDir()
	snap, err := NewSnapshotter(root, WithCubeUseRefPath)
	if err != nil {
		t.Fatalf("create snapshotter: %v", err)
	}
	o := snap.(*snapshotter)

	ctx := context.Background()
	if err := o.ms.WithTransaction(ctx, true, func(tx context.Context) error {
		_, err := storage.CreateSnapshot(tx, snapshots.KindActive, "key-keep", "", snapshots.WithLabels(map[string]string{
			constants.AnnotationSnapshotRef: constants.PrefixSha256 + "nsB",
		}))
		return err
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, refDirName, "nsA", "nsA"), 0o700); err != nil {
		t.Fatalf("create orphan ref: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, refDirName, "nsA", "nsB"), 0o700); err != nil {
		t.Fatalf("create kept ref: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, refDirName, "nsB"), 0o700); err != nil {
		t.Fatalf("create namespace dir: %v", err)
	}

	var cleanup []string
	if err := o.ms.WithTransaction(ctx, false, func(tx context.Context) error {
		var e error
		cleanup, e = o.getCleanupRefDirectories(tx)
		return e
	}); err != nil {
		t.Fatalf("get cleanup: %v", err)
	}
	expected := filepath.Join(root, refDirName, "nsA", "nsA")
	if len(cleanup) != 1 || cleanup[0] != expected {
		t.Fatalf("unexpected cleanup result: %v", cleanup)
	}
}

func TestRemoveDirectory(t *testing.T) {
	RequiresRoot(t)
	ctx := context.Background()

	t.Run("non mount path", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "plain")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		removeDirectory(ctx, dir)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("expected directory removed")
		}
	})

	t.Run("mount path", func(t *testing.T) {
		source := t.TempDir()
		target := filepath.Join(t.TempDir(), "mnt")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		mnts := []mount.Mount{{Type: "bind", Source: source, Options: []string{"bind"}}}
		if err := mount.All(mnts, target); err != nil {
			t.Fatalf("failed to mount: %v", err)
		}
		removeDirectory(ctx, target)
		if mounted, _ := utils.IsMountPoint(target); mounted {
			t.Fatalf("expected unmounted path")
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("expected target removed")
		}
	})
}
