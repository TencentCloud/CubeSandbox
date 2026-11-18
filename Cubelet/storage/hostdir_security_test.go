// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const testHostMountAllowedPrefixesAnnotation = "cube.master.internal.host_mount.allowed_prefixes"

func TestPrepareHostDirVolumeRejectsSymlinkEscape(t *testing.T) {
	allowedRoot := t.TempDir()
	outsideRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(allowedRoot, "tenant"), 0755))
	link := filepath.Join(allowedRoot, "tenant", "escape")
	require.NoError(t, os.Symlink(outsideRoot, link))

	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = t.TempDir()
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	origRun := runHostDirCommand
	t.Cleanup(func() { runHostDirCommand = origRun })
	var calls [][]string
	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	opts := hostDirTestOpts(t, []string{allowedRoot})
	err := (&local{}).prepareHostDirVolume(context.Background(), opts, hostDirTestVolume(link), &StorageInfo{})
	require.ErrorContains(t, err, "outside allowed host mount prefixes")
	require.Empty(t, calls, "must reject before invoking mount")
}

func TestPrepareHostDirVolumeRejectsSymlinkInsideAllowedPrefix(t *testing.T) {
	allowedRoot := t.TempDir()
	realDir := filepath.Join(allowedRoot, "tenant", "real")
	require.NoError(t, os.MkdirAll(realDir, 0755))
	link := filepath.Join(allowedRoot, "tenant", "alias")
	require.NoError(t, os.Symlink(realDir, link))

	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = t.TempDir()
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	origRun := runHostDirCommand
	t.Cleanup(func() { runHostDirCommand = origRun })
	var calls [][]string
	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	opts := hostDirTestOpts(t, []string{allowedRoot})
	err := (&local{}).prepareHostDirVolume(context.Background(), opts, hostDirTestVolume(link), &StorageInfo{})
	require.ErrorContains(t, err, "is a symlink below allowed host mount prefix")
	require.Empty(t, calls, "must reject before invoking mount")
}

// A prefix may itself sit below a symlinked directory that the operator
// configured (for example /data -> /mnt/ssd/data). Only links below the
// configured prefix are rejected.
func TestPrepareHostDirVolumeAllowsSymlinkedPrefixAncestor(t *testing.T) {
	realRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(realRoot, "shared", "tenant", "data"), 0755))
	aliasRoot := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.Symlink(realRoot, aliasRoot))
	prefix := filepath.Join(aliasRoot, "shared")
	hostPath := filepath.Join(prefix, "tenant", "data")

	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = t.TempDir()
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	origRun := runHostDirCommand
	t.Cleanup(func() { runHostDirCommand = origRun })
	var calls [][]string
	runHostDirCommand = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	opts := hostDirTestOpts(t, []string{prefix})
	err := (&local{}).prepareHostDirVolume(context.Background(), opts, hostDirTestVolume(hostPath), &StorageInfo{})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Contains(t, calls[0], "--no-canonicalize")

	var source string
	for _, arg := range calls[0] {
		if strings.HasPrefix(arg, "/proc/") {
			source = arg
			break
		}
	}
	require.True(t, strings.HasPrefix(source, fmt.Sprintf("/proc/%d/fd/", os.Getpid())),
		"mount source must be a pinned descriptor, got %q", source)
}

func TestPrepareHostDirVolumeRequiresAllowedPrefixesAnnotation(t *testing.T) {
	allowedRoot := t.TempDir()
	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = t.TempDir()
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	origRun := runHostDirCommand
	t.Cleanup(func() { runHostDirCommand = origRun })
	runHostDirCommand = func(ctx context.Context, name string, args ...string) error { return nil }

	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "hostdir-security-sb"},
		ReqInfo:          &cubebox.RunCubeSandboxRequest{},
	}
	err := (&local{}).prepareHostDirVolume(context.Background(), opts, hostDirTestVolume(allowedRoot), &StorageInfo{})
	require.ErrorContains(t, err, "missing "+testHostMountAllowedPrefixesAnnotation+" annotation")
}

func TestOpenHostDirSourcePinsDirectoryDescriptor(t *testing.T) {
	resolvedRoot := t.TempDir()
	source, err := openHostDirSource(resolvedRoot)
	require.NoError(t, err)
	defer source.Close()

	pinnedPath, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", source.Fd()))
	require.NoError(t, err)
	require.Equal(t, resolvedRoot, pinnedPath)
}

func TestOpenHostDirSourceRejectsSymlinkPath(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(target, link))

	_, err := openHostDirSource(link)
	require.Error(t, err)
}

// Exercise the real mount path: the descriptor is pinned, mount receives
// /proc/<pid>/fd/<n> with --no-canonicalize, and the mounted view must match
// the pinned directory. Also proves the read-only remount.
func TestBindHostDirMountsPinnedDescriptor(t *testing.T) {
	requireRoot(t)

	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "marker"), []byte("pinned"), 0644))
	dest := t.TempDir()

	sourceFile, err := openHostDirSource(source)
	require.NoError(t, err)
	defer sourceFile.Close()

	sourcePath := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), sourceFile.Fd())
	require.NoError(t, bindHostDir(context.Background(), sourcePath, dest, true))
	t.Cleanup(func() {
		require.NoError(t, unix.Unmount(dest, unix.MNT_DETACH))
	})

	data, err := os.ReadFile(filepath.Join(dest, "marker"))
	require.NoError(t, err)
	require.Equal(t, "pinned", string(data))

	err = os.WriteFile(filepath.Join(dest, "new-file"), []byte("x"), 0644)
	require.Error(t, err, "read-only remount must reject writes")
}

func TestResolveHostMountAllowedPrefixesRejectsRootSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Symlink("/", link))

	_, err := resolveHostMountAllowedPrefixes(map[string]string{
		testHostMountAllowedPrefixesAnnotation: mustJSON(t, []string{link}),
	})
	require.ErrorContains(t, err, "resolves to the filesystem root")
}

func TestResolveHostDirSourceRejectsSiblingPrefix(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "shared")
	sibling := filepath.Join(root, "shared-other")
	require.NoError(t, os.MkdirAll(allowed, 0755))
	require.NoError(t, os.MkdirAll(sibling, 0755))

	prefixes, err := resolveHostMountAllowedPrefixes(map[string]string{
		testHostMountAllowedPrefixesAnnotation: mustJSON(t, []string{allowed}),
	})
	require.NoError(t, err)

	_, err = resolveHostDirSource(sibling, prefixes)
	require.ErrorContains(t, err, "outside allowed host mount prefixes")
}

func TestResolveHostDirSourceRejectsSymlinkInsideAllowedPrefix(t *testing.T) {
	root := t.TempDir()
	tenantA := filepath.Join(root, "tenant-a")
	tenantB := filepath.Join(root, "tenant-b")
	require.NoError(t, os.MkdirAll(tenantA, 0755))
	require.NoError(t, os.MkdirAll(tenantB, 0755))
	link := filepath.Join(tenantA, "other-tenant")
	require.NoError(t, os.Symlink(tenantB, link))

	prefixes, err := resolveHostMountAllowedPrefixes(map[string]string{
		testHostMountAllowedPrefixesAnnotation: mustJSON(t, []string{root}),
	})
	require.NoError(t, err)

	_, err = resolveHostDirSource(link, prefixes)
	require.ErrorContains(t, err, "is a symlink below allowed host mount prefix")
}

// Spelling the path through the resolved form of a prefix must still walk the
// caller's components. Otherwise tenant-a -> tenant-b would resolve to
// tenant-b and leave no symlink to inspect.
func TestResolveHostDirSourceRejectsSymlinkViaResolvedPrefixSpelling(t *testing.T) {
	realRoot := t.TempDir()
	shared := filepath.Join(realRoot, "shared")
	tenantA := filepath.Join(shared, "tenant-a")
	tenantB := filepath.Join(shared, "tenant-b")
	require.NoError(t, os.MkdirAll(tenantA, 0755))
	require.NoError(t, os.MkdirAll(tenantB, 0755))
	require.NoError(t, os.Symlink(tenantB, filepath.Join(tenantA, "other-tenant")))

	aliasRoot := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.Symlink(realRoot, aliasRoot))
	configuredPrefix := filepath.Join(aliasRoot, "shared")

	prefixes, err := resolveHostMountAllowedPrefixes(map[string]string{
		testHostMountAllowedPrefixesAnnotation: mustJSON(t, []string{configuredPrefix}),
	})
	require.NoError(t, err)

	resolvedSpelling := filepath.Join(shared, "tenant-a", "other-tenant")
	_, err = resolveHostDirSource(resolvedSpelling, prefixes)
	require.ErrorContains(t, err, "is a symlink below allowed host mount prefix")
}

func hostDirTestOpts(t *testing.T, prefixes []string) *workflow.CreateContext {
	t.Helper()
	return &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "hostdir-security-sb"},
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			Annotations: map[string]string{
				testHostMountAllowedPrefixesAnnotation: mustJSON(t, prefixes),
			},
		},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

func hostDirTestVolume(hostPath string) *cubebox.Volume {
	return &cubebox.Volume{
		Name: "hostdir-0",
		VolumeSource: &cubebox.VolumeSource{
			HostDirVolumes: &cubebox.HostDirVolumeSources{
				VolumeSources: []*cubebox.HostDirSource{
					{Name: "data", HostPath: hostPath},
				},
			},
		},
	}
}
