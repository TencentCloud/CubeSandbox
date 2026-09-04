// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/stretchr/testify/require"
)

func TestWaitForTaskStabilityReturnsWhenTaskStaysAlive(t *testing.T) {
	calls := 0
	statusFn := func(context.Context) (containerd.Status, error) {
		calls++
		return containerd.Status{Status: containerd.Running}, nil
	}

	err := waitForTaskStability(context.Background(), statusFn, time.Millisecond)

	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestWaitForTaskStabilityRejectsExitedTask(t *testing.T) {
	statusFn := func(context.Context) (containerd.Status, error) {
		return containerd.Status{Status: containerd.Stopped, ExitStatus: 17}, nil
	}

	err := waitForTaskStability(context.Background(), statusFn, time.Second)

	require.Error(t, err)
	require.Contains(t, err.Error(), "exit_status=17")
}

func TestWaitForTaskStabilityPreservesExitError(t *testing.T) {
	statusFn := func(context.Context) (containerd.Status, error) {
		return containerd.Status{}, errors.New("status failed")
	}

	err := waitForTaskStability(context.Background(), statusFn, time.Second)

	require.Error(t, err)
	require.Contains(t, err.Error(), "status failed")
}
