// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"

	"github.com/stretchr/testify/require"
)

func TestCubeRuntimeSupportsKeepPaused(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         bool
		wantErr      bool
	}{
		{"legacy", "echo '  --app-snapshot app-snapshot'", false, false},
		{"supported", "if [ \"$1\" = snapshot ]; then echo '  --keep-paused keep paused'; else echo 'Usage: snapshot-resume'; fi", true, false},
		{"misleading description", "echo '  --app-snapshot use --keep-paused on newer versions'", false, false},
		{"broken help", "exit 2", false, true},
		{"missing resume", "if [ \"$1\" = snapshot ]; then echo '  --keep-paused keep paused'; else exit 2; fi", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimePath := filepath.Join(t.TempDir(), "cube-runtime")
			require.NoError(t, os.WriteFile(runtimePath, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755))
			got, err := cubeRuntimeSupportsKeepPaused(context.Background(), runtimePath)
			require.Equal(t, tc.want, got)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCubeRuntimeCapabilityCheckFailureDoesNotAllowLegacyFallback(t *testing.T) {
	_, err := cubeRuntimeSupportsKeepPaused(context.Background(), filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = cubeRuntimeSupportsKeepPaused(ctx, "/bin/sh")
	require.Error(t, err)
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	_, err = cubeRuntimeSupportsKeepPaused(deadlineCtx, "/bin/sh")
	require.Error(t, err)
	runtimePath := filepath.Join(t.TempDir(), "not-executable")
	require.NoError(t, os.WriteFile(runtimePath, []byte("#!/bin/sh\nexit 0\n"), 0o644))
	_, err = cubeRuntimeSupportsKeepPaused(context.Background(), runtimePath)
	require.Error(t, err)
}

func TestRunSnapshotWithRootfs(t *testing.T) {
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                              string
		keepPaused                        bool
		snapshotErr, rootfsErr, resumeErr error
		wantCalls                         []string
	}{
		{"new", true, nil, nil, nil, []string{"memory", "rootfs", "resume"}},
		{"legacy commit", false, nil, nil, nil, []string{"rootfs", "memory"}},
		{"new memory failure", true, failure, nil, nil, []string{"memory", "resume"}},
		{"new rootfs failure", true, nil, failure, nil, []string{"memory", "rootfs", "resume"}},
		{"new resume failure", true, nil, nil, failure, []string{"memory", "rootfs", "resume"}},
		{"legacy memory failure", false, failure, nil, nil, []string{"rootfs", "memory"}},
		{"legacy rootfs failure", false, nil, failure, nil, []string{"rootfs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			snapshotErr, rootfsErr, resumeErr := runSnapshotWithRootfs(tc.keepPaused,
				func() error { calls = append(calls, "memory"); return tc.snapshotErr },
				func() error { calls = append(calls, "rootfs"); return tc.rootfsErr },
				func() error { calls = append(calls, "resume"); return tc.resumeErr },
			)
			require.Equal(t, tc.wantCalls, calls)
			require.Equal(t, tc.snapshotErr, snapshotErr)
			require.Equal(t, tc.rootfsErr, rootfsErr)
			require.Equal(t, tc.resumeErr, resumeErr)
		})
	}
}

func TestAppSnapshotLegacyAndPausedRuntime(t *testing.T) {
	failure := errors.New("snapshot failed")
	for _, keepPaused := range []bool{false, true} {
		for _, memoryErr := range []error{nil, failure} {
			var calls []string
			snapshotErr, rootfsErr, resumeErr := runAppSnapshotWithRootfs(keepPaused,
				func() error {
					args := buildCubeRuntimeSnapshotArgsWithPause("sb", nil, "/tmp/meta", "/tmp/memory", snapshotTypeFull, keepPaused)
					require.Equal(t, keepPaused, slices.Contains(args, "--keep-paused"))
					calls = append(calls, "memory")
					return memoryErr
				},
				func() error { calls = append(calls, "rootfs"); return nil },
				func() error { calls = append(calls, "resume"); return nil },
			)
			want := []string{"memory"}
			if memoryErr == nil {
				want = append(want, "rootfs")
			}
			if keepPaused {
				want = append(want, "resume")
			}
			require.Equal(t, want, calls)
			require.Equal(t, memoryErr, snapshotErr)
			require.NoError(t, rootfsErr)
			require.NoError(t, resumeErr)
		}
	}
}

func TestCommitSnapshotDestinationRejectsExistingPackage(t *testing.T) {
	for _, name := range []string{"tpl-T-rootfs", "tpl-T-memory", "tpl-T-memory-snap", storage.S3MetadataSnapshotName("T")} {
		t.Run(name, func(t *testing.T) {
			err := checkCommitSnapshotDestination(context.Background(), "s3", "T",
				func(_ context.Context, backend string, refs []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
					require.Equal(t, "s3", backend)
					for _, ref := range refs {
						if ref.Name == name {
							return []storage.CowObjectStatus{{Name: name, Kind: ref.Kind, Exists: true}}, nil
						}
					}
					t.Fatalf("existing object %s was not checked", name)
					return nil, nil
				})
			require.ErrorIs(t, err, storage.ErrCowObjectAlreadyExists)
		})
	}
	dbErr := errors.New("inspect unavailable")
	err := checkCommitSnapshotDestination(context.Background(), "s3", "T",
		func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
			return nil, dbErr
		})
	require.ErrorIs(t, err, dbErr)
	require.NoError(t, checkCommitSnapshotDestination(context.Background(), "xfs", "new",
		func(context.Context, string, []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
			return nil, nil
		}))
}
