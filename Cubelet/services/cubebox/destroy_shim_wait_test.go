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
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// startSleeper starts a process that outlives the test body and returns its
// identity, so tests can exercise "still running" without racing the reaper.
func startSleeper(t *testing.T) utils.ProcessIdentity {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	id, err := utils.ReadProcessIdentity(cmd.Process.Pid)
	require.NoError(t, err)
	return id
}

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

func TestCollectEvidenceFindsLiveRecordedProcess(t *testing.T) {
	live := startSleeper(t)
	sb := newCubeboxWithStatusForTest("sb-live", cubeboxstore.Status{Pid: uint32(live.Pid), StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{
		Pid:          uint32(live.Pid),
		PidStartTime: live.StartTime,
		ShimSpawned:  true,
	}

	ev := (&local{}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.unresolved)
	assert.Equal(t, []int{live.Pid}, ev.pids(), "the live pid must be waited on, and only once")
}

func TestCollectEvidenceDropsExitedProcess(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-exited", cubeboxstore.Status{Pid: 1000000000, StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{Pid: 1000000000, PidStartTime: 42, ShimSpawned: true}

	ev := (&local{}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.identities)
	assert.Empty(t, ev.unresolved, "a recorded pid that is provably gone is conclusive, not unknown")
	require.NoError(t, waitSandboxRuntimeGone(context.Background(), "sb-exited", ev))
}

// A pid number that has been recycled must not make us wait on a stranger.
func TestCollectEvidenceIgnoresRecycledPid(t *testing.T) {
	live := startSleeper(t)
	sb := newCubeboxWithStatusForTest("sb-recycled", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{
		Pid:          uint32(live.Pid),
		PidStartTime: live.StartTime + 1, // same number, different incarnation
		ShimSpawned:  true,
	}

	ev := (&local{}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.identities)
	assert.Empty(t, ev.unresolved)
}

// The regression this whole change exists for: a shim was started but its pid
// never reached the store. There is no process to point at, and that absence
// must not be read as "safe to reclaim the tap and IP".
func TestCollectEvidenceFlagsSpawnedShimWithoutRecordedPid(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-orphan", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true}

	ev := (&local{}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.identities)
	require.NotEmpty(t, ev.unresolved)

	err := waitSandboxRuntimeGone(context.Background(), "sb-orphan", ev)
	require.Error(t, err, "empty evidence from an unresolved sandbox must not pass the gate")
	assert.Contains(t, err.Error(), "unresolved")
}

// Sandboxes created before this change have no ShimSpawned flag. They keep the
// old behaviour rather than becoming undeletable after an upgrade.
func TestCollectEvidenceAllowsPreUpgradeSandboxWithoutPid(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-legacy", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{}

	ev := (&local{}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.identities)
	assert.Empty(t, ev.unresolved)
	require.NoError(t, waitSandboxRuntimeGone(context.Background(), "sb-legacy", ev))
}

func TestWaitSandboxRuntimeGoneEmptyEvidence(t *testing.T) {
	require.NoError(t, waitSandboxRuntimeGone(context.Background(), "sb-empty", sandboxRuntimeEvidence{}))
}

func TestWaitSandboxRuntimeGoneBlocksUntilExit(t *testing.T) {
	cmd := exec.Command("sleep", "0.15")
	require.NoError(t, cmd.Start())
	id, err := utils.ReadProcessIdentity(cmd.Process.Pid)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev := sandboxRuntimeEvidence{identities: []utils.ProcessIdentity{id}}
	require.NoError(t, waitSandboxRuntimeGone(ctx, "sb-sleep", ev))
	_ = cmd.Wait()
}

func TestWaitSandboxRuntimeGoneTimesOut(t *testing.T) {
	live := startSleeper(t)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	ev := sandboxRuntimeEvidence{identities: []utils.ProcessIdentity{live}}
	require.Error(t, waitSandboxRuntimeGone(ctx, "sb-timeout", ev))
}

func TestSandboxShimLookupIDsDedups(t *testing.T) {
	sb := newCubeboxWithStatusForTest("same-id", cubeboxstore.Status{Pid: 9})
	assert.Equal(t, []string{"same-id"}, sandboxShimLookupIDs(sb))
}
