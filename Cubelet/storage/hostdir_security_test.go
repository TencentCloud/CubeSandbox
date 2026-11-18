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

func TestPrepareHostDirVolumeAllowsSymlinkInsideAllowedPrefix(t *testing.T) {
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

	_, err := resolveHostDirSource(sibling, []string{allowed})
	require.ErrorContains(t, err, "outside allowed host mount prefixes")
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
