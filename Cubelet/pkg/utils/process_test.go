// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessExists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flag := ProcessExists(ctx, 1)
	assert.True(t, flag)

	flag = ProcessExists(ctx, 1000000000)
	assert.False(t, flag)
}

func TestProcessAlive(t *testing.T) {
	assert.False(t, ProcessAlive(0))
	assert.False(t, ProcessAlive(1), "pid 1 is excluded so wait lists cannot pin init")
	assert.False(t, ProcessAlive(1000000000))
	assert.True(t, ProcessAlive(os.Getpid()))
}

func TestWaitProcessGoneMissingPID(t *testing.T) {
	require.NoError(t, WaitProcessGone(context.Background(), 0))
	require.NoError(t, WaitProcessGone(context.Background(), 1000000000))
}

func TestWaitProcessGoneWaitsThenSucceeds(t *testing.T) {
	cmd := exec.Command("sleep", "0.15")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.True(t, ProcessAlive(pid))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, WaitProcessGone(ctx, pid))
	assert.False(t, ProcessAlive(pid))
	_ = cmd.Wait()
}

func TestWaitProcessGoneHonorsCancel(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := WaitProcessGone(ctx, cmd.Process.Pid)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
