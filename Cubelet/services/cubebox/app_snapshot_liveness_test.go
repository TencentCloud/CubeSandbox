// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/stretchr/testify/require"
)

func TestVerifyTaskRunningAcceptsRunningTask(t *testing.T) {
	statusFn := func(context.Context) (containerd.Status, error) {
		return containerd.Status{Status: containerd.Running}, nil
	}

	err := verifyTaskRunning(context.Background(), statusFn)

	require.NoError(t, err)
}

func TestVerifyTaskRunningRejectsExitedTask(t *testing.T) {
	statusFn := func(context.Context) (containerd.Status, error) {
		return containerd.Status{Status: containerd.Stopped, ExitStatus: 17}, nil
	}

	err := verifyTaskRunning(context.Background(), statusFn)

	require.Error(t, err)
	require.Contains(t, err.Error(), "exit_status=17")
}

func TestVerifyTaskRunningPreservesStatusError(t *testing.T) {
	statusFn := func(context.Context) (containerd.Status, error) {
		return containerd.Status{}, errors.New("status failed")
	}

	err := verifyTaskRunning(context.Background(), statusFn)

	require.Error(t, err)
	require.Contains(t, err.Error(), "status failed")
}

func TestWaitSnapshotReadinessProbeReturnsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitSnapshotReadinessProbe(ctx, make(chan error))

	require.ErrorIs(t, err, context.Canceled)
}

func TestVerifyTaskAroundSnapshotProbeRejectsExitDuringProbe(t *testing.T) {
	checks := 0
	err := verifyTaskAroundSnapshotProbe(func() error {
		checks++
		if checks == 2 {
			return errors.New("task stopped")
		}
		return nil
	}, func() error {
		return nil
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "after probe")
	require.Contains(t, err.Error(), "task stopped")
	require.Equal(t, 2, checks)
}

func TestVerifyTaskAroundSnapshotProbeSkipsSecondCheckWithoutProbe(t *testing.T) {
	checks := 0
	err := verifyTaskAroundSnapshotProbe(func() error {
		checks++
		return nil
	}, nil)

	require.NoError(t, err)
	require.Equal(t, 1, checks)
}
