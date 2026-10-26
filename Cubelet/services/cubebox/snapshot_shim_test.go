// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
