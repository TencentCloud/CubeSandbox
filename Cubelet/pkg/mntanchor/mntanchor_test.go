// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux

package mntanchor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

var testPaths = Paths{
	ShimTopdir:    "/run/cube-containers/shared",
	ShimSandboxes: "/run/cube-containers/shared/sandboxes",
	HandleDir:     "/usr/local/services/cubetoolbox/cubeletmnt",
	HandleFile:    "/usr/local/services/cubetoolbox/cubeletmnt/mnt",
}

type fakeHost struct {
	selfNs, pid1Ns     string
	readlinkErr        error
	sameDir            bool
	sameDirErr         error
	mkdirErr           error
	noPodUID           bool
	created            []string
	checkedContainer   string
	checkedHostThrough string
}

func (f *fakeHost) deps() Deps {
	return Deps{
		Readlink: func(path string) (string, error) {
			if f.readlinkErr != nil {
				return "", f.readlinkErr
			}
			switch path {
			case "/proc/self/ns/mnt":
				return f.selfNs, nil
			case "/proc/1/ns/mnt":
				return f.pid1Ns, nil
			}
			return "", errors.New("unexpected readlink " + path)
		},
		MkdirAll: func(path string) error {
			if f.mkdirErr != nil {
				return f.mkdirErr
			}
			f.created = append(f.created, path)
			return nil
		},
		SameDir: func(containerPath, hostPath string) (bool, error) {
			f.checkedContainer, f.checkedHostThrough = containerPath, hostPath
			return f.sameDir, f.sameDirErr
		},
		Getenv: func(key string) string {
			if key != PodUIDEnv || f.noPodUID {
				return ""
			}
			return "pod-a"
		},
	}
}

// Cubelet started directly on the host keeps the namespace handle where it
// always did: there is nothing to restart out from under it.
func TestChooseOnHostKeepsLocalHandle(t *testing.T) {
	f := &fakeHost{selfNs: "mnt:[1]", pid1Ns: "mnt:[1]", sameDir: true}
	a := Choose(testPaths, f.deps())
	require.False(t, a.Host)
	require.Equal(t, testPaths.HandleFile, a.OpenPath())
}

// In a container that can see the host's processes and shares the handle
// directory with the host, the handle is kept in the host mount namespace so
// a container restart finds it again instead of minting a new namespace.
func TestChooseInContainerSharingHostDirUsesHostHandle(t *testing.T) {
	f := &fakeHost{selfNs: "mnt:[2]", pid1Ns: "mnt:[1]", sameDir: true}
	a := Choose(testPaths, f.deps())
	require.True(t, a.Host)
	require.Equal(t, "/proc/1/root"+testPaths.HandleFile, a.OpenPath())
	require.Equal(t, testPaths.HandleDir, f.checkedContainer)
	require.Equal(t, "/proc/1/root"+testPaths.HandleDir, f.checkedHostThrough)
}

func TestChooseFallsBackWhenHostCannotBeUsed(t *testing.T) {
	for name, f := range map[string]*fakeHost{
		"different host directory":  {selfNs: "mnt:[2]", pid1Ns: "mnt:[1]", sameDir: false},
		"host directory unreadable": {selfNs: "mnt:[2]", pid1Ns: "mnt:[1]", sameDirErr: os.ErrPermission},
		"namespaces unreadable":     {readlinkErr: os.ErrPermission},
		"handle dir not creatable":  {selfNs: "mnt:[2]", pid1Ns: "mnt:[1]", sameDir: true, mkdirErr: os.ErrPermission},
		"pod identity unknown":      {selfNs: "mnt:[2]", pid1Ns: "mnt:[1]", sameDir: true, noPodUID: true},
	} {
		t.Run(name, func(t *testing.T) {
			a := Choose(testPaths, f.deps())
			require.False(t, a.Host)
			require.Equal(t, testPaths.HandleFile, a.OpenPath())
		})
	}
}

// On a node Cubelet never ran on, the handle directory does not exist yet.
// Comparing it then fails, and the first namespace used to stay in the
// container, so the first restart lost every mount the sandboxes depend on.
// Choose creates the directory before comparing, with the real file system.
func TestChoosePinsOnFirstStartWhenHandleDirIsMissing(t *testing.T) {
	root := t.TempDir()
	paths := testPaths
	paths.HandleDir = filepath.Join(root, "cubetoolbox", "cubeletmnt")
	paths.HandleFile = filepath.Join(paths.HandleDir, "mnt")
	f := &fakeHost{selfNs: "mnt:[2]", pid1Ns: "mnt:[1]"}
	deps := f.deps()
	deps.MkdirAll = DefaultDeps().MkdirAll
	// The host sees the same directory (a hostPath mount), modelled here by
	// comparing the directory with itself.
	deps.SameDir = func(containerPath, _ string) (bool, error) { return SameDir(containerPath, containerPath) }

	a := Choose(paths, deps)
	require.True(t, a.Host)
	fi, err := os.Stat(paths.HandleDir)
	require.NoError(t, err)
	require.True(t, fi.IsDir())
}

// On the host nothing is created up front; the setup commands do it as before.
func TestChooseOnHostCreatesNothing(t *testing.T) {
	f := &fakeHost{selfNs: "mnt:[1]", pid1Ns: "mnt:[1]", sameDir: true}
	Choose(testPaths, f.deps())
	require.Empty(t, f.created)
}

// The original command sequence is kept byte for byte for the local handle.
func TestSetupCommandsLocalHandleUnchanged(t *testing.T) {
	cmds := Anchor{Paths: testPaths}.SetupCommands(4242)
	require.Equal(t, [][]string{
		{"mkdir", "-p", testPaths.ShimTopdir},
		{"mkdir", "-p", testPaths.ShimSandboxes},
		{"nsenter", "-t", "4242", "-m", "mount", "--make-rslave", "/"},
		{"nsenter", "-t", "4242", "-m", "mount", "--bind", "--make-shared", testPaths.ShimTopdir, testPaths.ShimTopdir},
		{"mkdir", "-p", testPaths.HandleDir},
		{"touch", testPaths.HandleFile},
		{"mount", "--bind", "--make-private", testPaths.HandleDir, testPaths.HandleDir},
		{"mount", "--bind", "/proc/4242/ns/mnt", testPaths.HandleFile},
		{"kill", "-9", "4242"},
	}, cmds)
	require.Empty(t, Anchor{Paths: testPaths}.StaleHandleCommands(),
		"the local handle keeps using in-process unmount")
}

// With the host handle, only the steps that create and pin the handle move
// into the host mount namespace; the namespace itself is set up as before.
func TestSetupCommandsHostHandlePinsInHostNamespace(t *testing.T) {
	host := func(args ...string) []string {
		return append([]string{"nsenter", "-t", "1", "-m", "--"}, args...)
	}
	a := Anchor{Paths: testPaths, Host: true}
	require.Equal(t, [][]string{
		{"mkdir", "-p", testPaths.ShimTopdir},
		{"mkdir", "-p", testPaths.ShimSandboxes},
		{"nsenter", "-t", "4242", "-m", "mount", "--make-rslave", "/"},
		{"nsenter", "-t", "4242", "-m", "mount", "--bind", "--make-shared", testPaths.ShimTopdir, testPaths.ShimTopdir},
		host("mkdir", "-p", testPaths.HandleDir),
		host("touch", testPaths.HandleFile),
		host("mount", "--bind", "--make-private", testPaths.HandleDir, testPaths.HandleDir),
		host("mount", "--bind", "/proc/4242/ns/mnt", testPaths.HandleFile),
		{"kill", "-9", "4242"},
	}, a.SetupCommands(4242))
	require.Equal(t, [][]string{
		host("umount", testPaths.HandleFile),
		host("umount", testPaths.HandleDir),
	}, a.StaleHandleCommands())
}

// Reuse is decided by whether the path is a live mount namespace handle,
// which works the same through /proc/1/root as locally.
func TestIsMntNsHandle(t *testing.T) {
	require.True(t, IsMntNsHandle("/proc/self/ns/mnt"))
	require.False(t, IsMntNsHandle("/proc/self/ns/net"), "a non-mount namespace is not a cubelet mount handle")

	plain := filepath.Join(t.TempDir(), "mnt")
	require.NoError(t, os.WriteFile(plain, nil, 0o600))
	require.False(t, IsMntNsHandle(plain), "an empty placeholder left by a restarted container is not a handle")
	require.False(t, IsMntNsHandle(filepath.Join(t.TempDir(), "missing")))
}

func TestSameDirComparesDeviceAndInode(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(dir, link))

	same, err := SameDir(dir, link)
	require.NoError(t, err)
	require.True(t, same)

	same, err = SameDir(dir, t.TempDir())
	require.NoError(t, err)
	require.False(t, same)

	_, err = SameDir(dir, filepath.Join(dir, "missing"))
	require.Error(t, err)
}

// A host-pinned namespace is rooted in the file system of the container that
// created it. It is re-entered only by later containers of the same pod; a
// replaced pod (new volumes, new spec) creates its own namespace.
func TestReusableRequiresSamePodForHostHandle(t *testing.T) {
	live := func(string) bool { return true }
	dead := func(string) bool { return false }
	owner := func(content string, err error) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			require.Equal(t, "/proc/1/root"+testPaths.HandleDir+"/mnt.owner", path)
			return []byte(content), err
		}
	}
	host := Anchor{Paths: testPaths, Host: true, Owner: "pod-a"}
	require.True(t, host.Reusable(live, owner("pod-a\n", nil)))
	require.False(t, host.Reusable(live, owner("pod-b\n", nil)), "handle of a replaced pod")
	require.False(t, host.Reusable(live, owner("", os.ErrNotExist)), "handle with no recorded pod")
	require.False(t, host.Reusable(dead, owner("pod-a", nil)))

	local := Anchor{Paths: testPaths}
	require.True(t, local.Reusable(live, owner("", os.ErrNotExist)))
	require.False(t, local.Reusable(dead, nil))
}

func TestRecordOwnerOnlyForHostHandle(t *testing.T) {
	var wrote string
	write := func(path string, data []byte, _ os.FileMode) error {
		wrote = path + "=" + string(data)
		return nil
	}
	require.NoError(t, Anchor{Paths: testPaths}.RecordOwner(write))
	require.Empty(t, wrote)
	require.NoError(t, Anchor{Paths: testPaths, Host: true, Owner: "pod-a"}.RecordOwner(write))
	require.Equal(t, "/proc/1/root"+testPaths.HandleDir+"/mnt.owner=pod-a\n", wrote)
}
