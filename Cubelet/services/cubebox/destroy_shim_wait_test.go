// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestReadPidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shim.pid")
	require.NoError(t, os.WriteFile(path, []byte("  4321\n"), 0o644))

	assert.Equal(t, 4321, readPidFile(path))
	assert.Zero(t, readPidFile(filepath.Join(dir, "missing.pid")))
	assert.Zero(t, readPidFile(""))
	require.NoError(t, os.WriteFile(path, []byte("not-a-pid"), 0o644))
	assert.Zero(t, readPidFile(path))
}

func TestCollectSandboxRuntimePIDsUsesRecordedPids(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-wait", cubeboxstore.Status{Pid: 4242, StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{Pid: 4242}

	l := &local{}
	assert.Equal(t, []int{4242}, l.collectSandboxRuntimePIDs(context.Background(), sb))
}

func TestCollectSandboxRuntimePIDsReadsBundlePidFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, shimPidFileName), []byte("88001"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, vmmPidFileName), []byte("88002"), 0o644))

	sb := newCubeboxWithStatusForTest("sb-bundle", cubeboxstore.Status{Pid: 88001, StartedAt: 1})
	pids := recordedSandboxPIDs(sb)
	require.Contains(t, pids, 88001)

	got := map[int]struct{}{}
	for _, pid := range []int{
		readPidFile(filepath.Join(dir, shimPidFileName)),
		readPidFile(filepath.Join(dir, vmmPidFileName)),
	} {
		if pid > 1 {
			got[pid] = struct{}{}
		}
	}
	assert.Contains(t, got, 88001)
	assert.Contains(t, got, 88002)
}

func TestWaitSandboxRuntimeGoneNoPIDs(t *testing.T) {
	require.NoError(t, waitSandboxRuntimeGone(context.Background(), "sb-empty", nil))
}

func TestWaitSandboxRuntimeGoneBlocksUntilExit(t *testing.T) {
	cmd := exec.Command("sleep", "0.15")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, waitSandboxRuntimeGone(ctx, "sb-sleep", []int{pid}))
	_ = cmd.Wait()
}

func TestWaitSandboxRuntimeGoneTimesOut(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := waitSandboxRuntimeGone(ctx, "sb-timeout", []int{cmd.Process.Pid})
	require.Error(t, err)
}

func TestSandboxShimLookupIDsDedups(t *testing.T) {
	sb := newCubeboxWithStatusForTest("same-id", cubeboxstore.Status{Pid: 9})
	assert.Equal(t, []string{"same-id"}, sandboxShimLookupIDs(sb))
}
