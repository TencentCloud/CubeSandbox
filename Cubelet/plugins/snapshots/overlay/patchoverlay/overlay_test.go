// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package patchoverlay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/core/snapshots/testsuite"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay/overlayutils"
	"github.com/opencontainers/runtime-spec/specs-go"
)

var rootEnabled = true

func RequiresRoot(t testing.TB) {
	if !rootEnabled {
		t.Skip("skipping test that requires root")
		return
	}
	if os.Getuid() != 0 {
		t.Skip("skipping test that requires root")
	}
}

func newSnapshotterWithOpts(opts ...Opt) testsuite.SnapshotterFunc {
	return func(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
		snapshotter, err := NewSnapshotter(root, opts...)
		if err != nil {
			return nil, nil, err
		}

		return snapshotter, func() error { return snapshotter.Close() }, nil
	}
}

func TestOverlay(t *testing.T) {
	RequiresRoot(t)
	optTestCases := map[string][]Opt{
		"no opt": nil,

		"AsynchronousRemove": {AsynchronousRemove},

		"WithRemapIDs":       {WithRemapIDs},
		"WithCubeUseRefPath": {WithCubeUseRefPath},
	}

	for optsName, opts := range optTestCases {
		t.Run(optsName, func(t *testing.T) {
			newSnapshotter := newSnapshotterWithOpts(opts...)
			testsuite.SnapshotterSuite(t, "overlayfs", newSnapshotter)
			t.Run("TestOverlayRemappedInvalidMappings", func(t *testing.T) {
				testOverlayRemappedInvalidMapping(t, newSnapshotter)
			})
			t.Run("TestOverlayMounts", func(t *testing.T) {
				testOverlayMounts(t, newSnapshotter)
			})
			t.Run("TestOverlayCommit", func(t *testing.T) {
				testOverlayCommit(t, newSnapshotter)
			})
			t.Run("TestOverlayOverlayMount", func(t *testing.T) {
				testOverlayOverlayMount(t, newSnapshotter)
			})
			t.Run("TestOverlayOverlayRead", func(t *testing.T) {
				testOverlayOverlayRead(t, newSnapshotter)
			})
			t.Run("TestOverlayView", func(t *testing.T) {
				testOverlayView(t, newSnapshotterWithOpts(append(opts, WithMountOptions([]string{"volatile"}))...))
			})
		})
	}
}

func testOverlayMounts(t *testing.T, newSnapshotter testsuite.SnapshotterFunc) {
	ctx := context.TODO()
	root := t.TempDir()
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := o.Prepare(ctx, "/tmp/test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "bind" {
		t.Errorf("mount type should be bind but received %q", m.Type)
	}
	expected := filepath.Join(root, "snapshots", "1", "fs")
	if m.Source != expected {
		t.Errorf("expected source %q but received %q", expected, m.Source)
	}
	if m.Options[0] != "rw" {
		t.Errorf("expected mount option rw but received %q", m.Options[0])
	}
	if m.Options[1] != "rbind" {
		t.Errorf("expected mount option rbind but received %q", m.Options[1])
	}
}

func testOverlayCommit(t *testing.T, newSnapshotter testsuite.SnapshotterFunc) {
	ctx := context.TODO()
	root := t.TempDir()
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := os.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}

	activeParent := "active-parent"
	if _, err := o.Prepare(ctx, activeParent, ""); err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		oldParent string
		newParent string
		expError  bool
	}{
		{
			oldParent: "",
			newParent: "",
			expError:  false,
		},
		{
			oldParent: "",
			newParent: "base",
			expError:  false,
		},
		{
			oldParent: "base",
			newParent: "base",
			expError:  false,
		},
		{
			oldParent: "base",
			newParent: "",
			expError:  false,
		},
		{
			oldParent: "base",
			newParent: "new",
			expError:  true,
		},
		{
			oldParent: "",
			newParent: activeParent,
			expError:  true,
		},
	}
	for i, tc := range testCases {
		key := fmt.Sprintf("/tmp/test-%d", i)
		name := fmt.Sprintf("test-%d", i)
		_, err := o.Prepare(ctx, key, tc.oldParent)
		if err != nil {
			t.Fatal(err)
		}
		if err := o.Commit(ctx, name, key, snapshots.WithParent(tc.newParent)); err != nil {
			if !tc.expError {
				t.Fatal(err)
			}
			t.Logf("expected error received: %v", err)
		} else if tc.expError {
			t.Fatal("expected error but commit succeeded")
		}
	}
}

func testOverlayOverlayMount(t *testing.T, newSnapshotter testsuite.SnapshotterFunc) {
	ctx := context.TODO()
	root := t.TempDir()
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	if _, err = o.Prepare(ctx, key, ""); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
	var mounts []mount.Mount
	if mounts, err = o.Prepare(ctx, "/tmp/layer2", "base"); err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("expected source %q but received %q", "overlay", m.Source)
	}
	var (
		expected []string
		bp       = getBasePath(ctx, o, root, "/tmp/layer2")
		work     = "workdir=" + filepath.Join(bp, "work")
		upper    = "upperdir=" + filepath.Join(bp, "fs")
		lower    = "lowerdir=" + getParents(ctx, o, root, "/tmp/layer2")[0]
	)

	expected = append(expected, []string{
		work,
		upper,
		lower,
	}...)

	if supportsIndex() {
		expected = append(expected, "index=off")
	}
	if userxattr, err := overlayutils.NeedsUserXAttr(root); err != nil {
		t.Fatal(err)
	} else if userxattr {
		expected = append(expected, "userxattr")
	}

	for i, v := range expected {
		if m.Options[i] != v {
			t.Errorf("expected %q but received %q", v, m.Options[i])
		}
	}
}

func testOverlayRemappedInvalidMapping(t *testing.T, newSnapshotter testsuite.SnapshotterFunc) {
	ctx := context.TODO()
	root := t.TempDir()
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}

	if sn, ok := o.(*snapshotter); !ok || !sn.remapIDs {
		t.Skip("overlayfs doesn't support idmapped mounts")
	}

	key := "/tmp/test"
	for desc, opts := range map[string][]snapshots.Opt{
		"WithLabels: negative UID mapping must fail": {
			snapshots.WithLabels(map[string]string{
				snapshots.LabelSnapshotUIDMapping: "-1:-1:-2",
				snapshots.LabelSnapshotGIDMapping: "0:0:66666",
			}),
		},
		"WithLabels: negative GID mapping must fail": {
			snapshots.WithLabels(map[string]string{
				snapshots.LabelSnapshotUIDMapping: "0:0:66666",
				snapshots.LabelSnapshotGIDMapping: "-1:-1:-2",
			}),
		},
		"WithLabels: negative GID/UID mappings must fail": {
			snapshots.WithLabels(map[string]string{
				snapshots.LabelSnapshotUIDMapping: "-666:-666:-666",
				snapshots.LabelSnapshotGIDMapping: "-666:-666:-666",
			}),
		},
		"WithLabels: negative UID in multiple mappings must fail": {
			snapshots.WithLabels(map[string]string{
				snapshots.LabelSnapshotUIDMapping: "1:1:1,-1:-1:-2",
				snapshots.LabelSnapshotGIDMapping: "0:0:66666",
			}),
		},
		"WithLabels: negative GID in multiple mappings must fail": {
			snapshots.WithLabels(map[string]string{
				snapshots.LabelSnapshotUIDMapping: "0:0:66666",
				snapshots.LabelSnapshotGIDMapping: "-1:-1:-2,6:6:6",
			}),
		},
		"WithLabels: negative GID/UID in multiple mappings must fail": {
			snapshots.WithLabels(map[string]string{
				snapshots.LabelSnapshotUIDMapping: "-666:-666:-666,1:1:1",
				snapshots.LabelSnapshotGIDMapping: "-666:-666:-666,2:2:2",
			}),
		},
		"WithRemapperLabels: container ID (GID/UID) other than 0 must fail": {
			containerd.WithRemapperLabels(666, 666, 666, 666, 666),
		},
		"WithRemapperLabels: container ID (UID) other than 0 must fail": {
			containerd.WithRemapperLabels(666, 0, 0, 0, 65536),
		},
		"WithRemapperLabels: container ID (GID) other than 0 must fail": {
			containerd.WithRemapperLabels(0, 0, 666, 0, 4294967295),
		},
		"WithUserNSRemapperLabels: container ID (GID/UID) other than 0 must fail": {
			containerd.WithUserNSRemapperLabels(
				[]specs.LinuxIDMapping{{ContainerID: 666, HostID: 666, Size: 666}},
				[]specs.LinuxIDMapping{{ContainerID: 666, HostID: 666, Size: 666}},
			),
		},
		"WithUserNSRemapperLabels: container ID (UID) other than 0 must fail": {
			containerd.WithUserNSRemapperLabels(
				[]specs.LinuxIDMapping{{ContainerID: 666, HostID: 0, Size: 65536}},
				[]specs.LinuxIDMapping{{ContainerID: 0, HostID: 0, Size: 65536}},
			),
		},
		"WithUserNSRemapperLabels: container ID (GID) other than 0 must fail": {
			containerd.WithUserNSRemapperLabels(
				[]specs.LinuxIDMapping{{ContainerID: 0, HostID: 0, Size: 4294967295}},
				[]specs.LinuxIDMapping{{ContainerID: 666, HostID: 0, Size: 4294967295}},
			),
		},
	} {
		t.Log(desc)
		if _, err = o.Prepare(ctx, key, "", opts...); err == nil {
			t.Fatalf("snapshots with invalid mappings must fail")
		}

		_ = o.Remove(ctx, key)
	}
}

func getBasePath(ctx context.Context, sn snapshots.Snapshotter, root, key string) string {
	o := sn.(*snapshotter)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		panic(err)
	}
	defer t.Rollback()

	s, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		panic(err)
	}

	return filepath.Join(root, "snapshots", s.ID)
}

func getParents(ctx context.Context, sn snapshots.Snapshotter, root, key string) []string {
	o := sn.(*snapshotter)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		panic(err)
	}
	defer t.Rollback()
	s, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		panic(err)
	}
	parents := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parents[i] = filepath.Join(root, "snapshots", s.ParentIDs[i], "fs")
	}
	return parents
}

func testOverlayOverlayRead(t *testing.T, newSnapshotter testsuite.SnapshotterFunc) {
	ctx := context.TODO()
	root := t.TempDir()
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := os.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
	if mounts, err = o.Prepare(ctx, "/tmp/layer2", "base"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := mount.All(mounts, dest); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount(dest, 0)
	data, err := os.ReadFile(filepath.Join(dest, "foo"))
	if err != nil {
		t.Fatal(err)
	}
	if e := string(data); e != "hi" {
		t.Fatalf("expected file contents hi but got %q", e)
	}
}

func testOverlayView(t *testing.T, newSnapshotter testsuite.SnapshotterFunc) {
	ctx := context.TODO()
	root := t.TempDir()
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/base"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := os.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}

	key = "/tmp/top"
	_, err = o.Prepare(ctx, key, "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(getParents(ctx, o, root, "/tmp/top")[0], "foo"), []byte("hi, again"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "top", key); err != nil {
		t.Fatal(err)
	}

	mounts, err = o.View(ctx, "/tmp/view1", "base")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("should only have 1 mount but received %d", len(mounts))
	}
	m = mounts[0]
	if m.Type != "bind" {
		t.Errorf("mount type should be bind but received %q", m.Type)
	}
	expected := getParents(ctx, o, root, "/tmp/view1")[0]
	if m.Source != expected {
		t.Errorf("expected source %q but received %q", expected, m.Source)
	}

	if m.Options[0] != "ro" {
		t.Errorf("expected mount option ro but received %q", m.Options[0])
	}
	if m.Options[1] != "rbind" {
		t.Errorf("expected mount option rbind but received %q", m.Options[1])
	}

	mounts, err = o.View(ctx, "/tmp/view2", "top")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("should only have 1 mount but received %d", len(mounts))
	}
	m = mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("mount source should be overlay but received %q", m.Source)
	}

	supportsIndex := supportsIndex()
	expectedOptions := 3
	if !supportsIndex {
		expectedOptions--
	}
	userxattr, err := overlayutils.NeedsUserXAttr(root)
	if err != nil {
		t.Fatal(err)
	}
	if userxattr {
		expectedOptions++
	}

	if len(m.Options) != expectedOptions {
		t.Errorf("expected %d additional mount option but got %d", expectedOptions, len(m.Options))
	}
	lowers := getParents(ctx, o, root, "/tmp/view2")

	expected = fmt.Sprintf("lowerdir=%s:%s", lowers[0], lowers[1])
	if m.Options[0] != expected {
		t.Errorf("expected option %q but received %q", expected, m.Options[0])
	}

	if m.Options[1] != "volatile" {
		t.Error("expected option first option to be provided option \"volatile\"")
	}
}
