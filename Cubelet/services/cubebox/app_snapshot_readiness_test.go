// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestRunDeclaredAppSnapshotReadinessProbeRunsDeclaredProbeOnce(t *testing.T) {
	calls := 0
	err := runDeclaredAppSnapshotReadinessProbe(&cubebox.ContainerConfig{
		Probe: &cubebox.Probe{ProbeHandler: &cubebox.ProbeHandler{
			Ping: &cubebox.PingAction{},
		}},
	}, func() error {
		calls++
		return errors.New("not ready")
	})

	require.EqualError(t, err, "not ready")
	require.Equal(t, 1, calls)
}

func TestRunDeclaredAppSnapshotReadinessProbeSkipsTemplatesWithoutProbe(t *testing.T) {
	calls := 0
	err := runDeclaredAppSnapshotReadinessProbe(&cubebox.ContainerConfig{}, func() error {
		calls++
		return errors.New("must not run")
	})

	require.NoError(t, err)
	require.Zero(t, calls)
}

func TestWaitSnapshotReadinessProbeReturnsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitSnapshotReadinessProbe(ctx, make(chan error))

	require.ErrorIs(t, err, context.Canceled)
}
