// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func TestReadProcessIdentity(t *testing.T) {
	self, err := ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), self.Pid)
	assert.NotZero(t, self.StartTime, "start time is what pins a pid to one incarnation")

	again, err := ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, self.StartTime, again.StartTime, "start time must be stable across reads")

	_, err = ReadProcessIdentity(0)
	assert.Error(t, err)
	_, err = ReadProcessIdentity(1000000000)
	assert.Error(t, err)
}

// The command name is embedded in /proc/<pid>/stat inside parentheses and can
// contain both spaces and parentheses, which breaks naive field splitting.
func TestReadProcessIdentityHandlesCommWithSpacesAndParens(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "we ird (name)")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755))

	cmd := exec.Command(script)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	id, err := ReadProcessIdentity(cmd.Process.Pid)
	require.NoError(t, err)
	assert.NotZero(t, id.StartTime)
	assert.Equal(t, LivenessAlive, id.Status())
}

func TestProcessIdentityStatus(t *testing.T) {
	self, err := ReadProcessIdentity(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, LivenessAlive, self.Status())

	// A live pid whose start time does not match is a recycled number, not
	// our process. Reporting it as alive would make callers wait forever on a
	// stranger; reporting it as gone is what lets them make progress.
	recycled := ProcessIdentity{Pid: self.Pid, StartTime: self.StartTime + 1}
	assert.Equal(t, LivenessGone, recycled.Status())

	// No start time recorded: the pid is alive but we cannot say whose it is.
	// This must not be reported as gone.
	assert.Equal(t, LivenessUnknown, ProcessIdentity{Pid: self.Pid}.Status())

	assert.Equal(t, LivenessGone, ProcessIdentity{Pid: 1000000000, StartTime: 1}.Status())
	assert.Equal(t, LivenessGone, ProcessIdentity{Pid: 0}.Status())
}

func TestWaitIdentityGone(t *testing.T) {
	cmd := exec.Command("sleep", "0.15")
	require.NoError(t, cmd.Start())
	id, err := ReadProcessIdentity(cmd.Process.Pid)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, WaitIdentityGone(ctx, id))
	_ = cmd.Wait()
}

func TestWaitIdentityGoneRefusesUnknown(t *testing.T) {
	// A pid with no recorded start time is unknown, not gone. WaitProcessGone
	// would happily return nil here once the number looked free; this must not.
	err := WaitIdentityGone(context.Background(), ProcessIdentity{Pid: os.Getpid()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown")
}

func TestWaitIdentityGoneHonorsCancel(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	id, err := ReadProcessIdentity(cmd.Process.Pid)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err = WaitIdentityGone(ctx, id)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
