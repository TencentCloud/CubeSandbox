// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

func TestShimSnapshotUnsupportedOnlyMatchesLegacyActionError(t *testing.T) {
	require.True(t, shimSnapshotUnsupported(fmt.Errorf("update shim: %w", errors.New("unknown update ext action: SnapshotCapture"))))
	for _, err := range []error{
		nil,
		errors.New("snapshot capture failed after VM pause"),
		errors.New("unknown update ext action: SnapshotResume"),
	} {
		require.False(t, shimSnapshotUnsupported(err))
	}
}

func TestSnapshotCaptureErrorReportsIncompatibleShim(t *testing.T) {
	legacyErr := errors.New("unknown update ext action: SnapshotCapture")
	require.ErrorIs(t, snapshotCaptureError(fmt.Errorf("update shim: %w", legacyErr)), errSnapshotShimIncompatible)
	require.Equal(t, "incompatible CubeShim: SnapshotCapture is unavailable; upgrade the sandbox shim to create a consistent memory/rootfs snapshot", errSnapshotShimIncompatible.Error())
	captureErr := errors.New("capture failed")
	require.ErrorIs(t, snapshotCaptureError(captureErr), captureErr)
}

func TestLegacyPathRefusesFilesystemOnlyWithAClassifiableSentinel(t *testing.T) {
	// The user-visible text is unchanged: only the error's identity became
	// classifiable so it can be reported with the stable identifier.
	require.EqualError(t, errFilesystemOnlyNeedsCoordinatedShim,
		"filesystem-only snapshot requires the coordinated shim path")
}

func TestFilesystemOnlyUnsupportedMessage(t *testing.T) {
	const version = "v0.7.3-fs-snapshot-1"
	const want = `snapshot_filesystem_only_unsupported_node: shim "v0.7.3-fs-snapshot-1" does not support filesystem-only snapshots`

	t.Run("shim_rejected_the_coordinated_action", func(t *testing.T) {
		// The shape produced by captureSnapshotWithShim -> snapshotCaptureError,
		// wrapped once more by CommitSandbox before it is inspected.
		err := fmt.Errorf("failed to capture sandbox snapshot: %w", snapshotCaptureError(
			fmt.Errorf("update shim: %w", errors.New("unknown update ext action: SnapshotCapture"))))
		require.Equal(t, want, filesystemOnlyUnsupportedMessage(err, version, snapshotmode.FsOnly))
	})

	t.Run("legacy_path_refused_the_capture", func(t *testing.T) {
		err := fmt.Errorf("wrapped twice: %w", fmt.Errorf("inner: %w", errFilesystemOnlyNeedsCoordinatedShim))
		require.Equal(t, want, filesystemOnlyUnsupportedMessage(err, version, snapshotmode.FsOnly))
	})

	t.Run("unknown_pin_reports_an_empty_version", func(t *testing.T) {
		require.Equal(t,
			`snapshot_filesystem_only_unsupported_node: shim "" does not support filesystem-only snapshots`,
			filesystemOnlyUnsupportedMessage(errFilesystemOnlyNeedsCoordinatedShim, "", snapshotmode.FsOnly))
	})

	t.Run("full_requests_keep_their_own_failure", func(t *testing.T) {
		err := snapshotCaptureError(fmt.Errorf("update shim: %w", errors.New("unknown update ext action: SnapshotCapture")))
		require.Empty(t, filesystemOnlyUnsupportedMessage(err, version, snapshotmode.Full))
	})

	t.Run("other_failures_are_not_reported_as_unsupported", func(t *testing.T) {
		for _, err := range []error{
			nil,
			errors.New("snapshot capture failed while freezing the rootfs"),
			fmt.Errorf("synthetic: %s", snapshotmode.ErrFilesystemOnlyFreezeFailed),
		} {
			require.Empty(t, filesystemOnlyUnsupportedMessage(err, version, snapshotmode.FsOnly), "err=%v", err)
		}
	})
}

func TestSnapshotRenewalFailureRejectsCompletedRootfs(t *testing.T) {
	renewErr := errors.New("shim update unavailable")
	workCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var lease *snapshotFreezeLease
	var rootfsWorkErr error
	snapshotErr, rootfsErr, resumeErr := runSnapshotWithRootfs(func() error {
		lease = newSnapshotFreezeLease(time.Millisecond, func() error { return renewErr }, cancel)
		return nil
	}, func() error {
		// Model a rootfs commit that completes after the freeze was lost.
		<-workCtx.Done()
		rootfsWorkErr = workCtx.Err()
		return nil
	}, func() error {
		// Even if SnapshotResume reports success after automatic recovery,
		// the failed renewal must prevent publication.
		return errors.Join(lease.Stop(), nil)
	})
	require.NoError(t, snapshotErr)
	require.NoError(t, rootfsErr)
	require.ErrorIs(t, rootfsWorkErr, context.Canceled)
	require.ErrorIs(t, resumeErr, renewErr)
}

// A shim that predates filesystem-only captures accepts the action but ignores
// the mode, so it pauses the VM and writes guest memory. The artifact is the
// only reliable signal: a memory capture always writes snapshot/state.json.
func TestCapturedGuestMemoryDetectsTheFullPathArtifact(t *testing.T) {
	dir := t.TempDir()
	captured, err := capturedGuestMemory(dir)
	require.NoError(t, err)
	require.False(t, captured, "an empty capture dir has no VM state")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "snapshot"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot", "config.json"), []byte("{}"), 0o644))
	captured, err = capturedGuestMemory(dir)
	require.NoError(t, err)
	require.False(t, captured, "a filesystem-only capture writes only config.json")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot", "state.json"), []byte("{}"), 0o644))
	captured, err = capturedGuestMemory(dir)
	require.NoError(t, err)
	require.True(t, captured)
}

// A capture directory that cannot be inspected is a local failure, not evidence
// about shim capability: reporting it as captured memory would tell the operator
// to upgrade a shim that may well be current.
func TestCapturedGuestMemoryReportsAnUninspectableCapture(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the capture directory belongs makes every stat below
	// it fail with ENOTDIR, for any uid.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot"), []byte("not a directory"), 0o644))

	captured, err := capturedGuestMemory(dir)
	require.Error(t, err)
	require.False(t, captured)

	svc := &service{}
	captureErr := svc.captureSnapshotWithShim(context.Background(), nil, "snap-1", dir, "", "pause", "/dev/vdb", snapshotmode.FsOnly)
	require.NotErrorIs(t, captureErr, errFilesystemOnlyCapturedMemory)
	require.Empty(t, filesystemOnlyUnsupportedMessage(captureErr, "v0.7.3", snapshotmode.FsOnly))
	require.Contains(t, captureErr.Error(), "inspect snapshot capture")
}

func TestFilesystemOnlyUnsupportedMessageCoversTheMemoryArtifact(t *testing.T) {
	const version = "v0.7.3"
	got := filesystemOnlyUnsupportedMessage(errFilesystemOnlyCapturedMemory, version, snapshotmode.FsOnly)
	require.Equal(t,
		`snapshot_filesystem_only_unsupported_node: shim "v0.7.3" does not support filesystem-only snapshots`,
		got)
	// The same artifact must not be reported for a full request: there, guest
	// memory is exactly what was asked for.
	require.Empty(t,
		filesystemOnlyUnsupportedMessage(errFilesystemOnlyCapturedMemory, version, snapshotmode.Full))
}

// The artifact decides, not the shim's reply: a shim that ignores snapshot_mode
// writes guest memory while reporting success. This drives the real entry point
// (not the helper) so that deleting the guard turns the test red.
//
// A nil sandbox makes updateSnapshotShim fail before it touches containerd,
// which is exactly the "capture did not run cleanly" shape the guard must
// override when the shim has already written the memory artifact.
func TestCaptureSnapshotWithShimChecksTheArtifactItGot(t *testing.T) {
	ctx := context.Background()
	withMemory := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withMemory, "snapshot"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(withMemory, "snapshot", "state.json"), []byte("{}"), 0o644))

	svc := &service{}
	err := svc.captureSnapshotWithShim(ctx, nil, "snap-1", withMemory, "", "pause", "/dev/vdb", snapshotmode.FsOnly)
	require.ErrorIs(t, err, errFilesystemOnlyCapturedMemory,
		"a filesystem-only request that produced guest memory must be refused")

	err = svc.captureSnapshotWithShim(ctx, nil, "snap-1", withMemory, "", "pause", "/dev/vdb", snapshotmode.Full)
	require.NotErrorIs(t, err, errFilesystemOnlyCapturedMemory,
		"full captures legitimately write memory")
	require.Error(t, err, "the nil sandbox still fails")

	withoutMemory := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withoutMemory, "snapshot"), 0o755))
	err = svc.captureSnapshotWithShim(ctx, nil, "snap-1", withoutMemory, "", "pause", "/dev/vdb", snapshotmode.FsOnly)
	require.NotErrorIs(t, err, errFilesystemOnlyCapturedMemory,
		"a filesystem-only capture without memory keeps the shim's own error")
	require.Error(t, err)
}
