// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

func writePidFile(t *testing.T, path string, pid int) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644))
}

func TestReapStartupOrphanShimsSkipsKnownAndKillsOrphan(t *testing.T) {
	root := t.TempDir()

	// Known sandbox: bundle exists but the store already tracks it.
	knownID := "sb-known"
	require.NoError(t, os.MkdirAll(filepath.Join(root, knownID), 0o755))
	sleeper := exec.Command("sleep", "10")
	require.NoError(t, sleeper.Start())
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	writePidFile(t, filepath.Join(root, knownID, shimPidFileName), sleeper.Process.Pid)

	// Orphan sandbox: bundle exists, VMM has already exited, shim is alive.
	orphanID := "sb-orphan"
	require.NoError(t, os.MkdirAll(filepath.Join(root, orphanID), 0o755))
	victim := exec.Command("sleep", "20")
	require.NoError(t, victim.Start())
	t.Cleanup(func() { _ = victim.Process.Kill(); _ = victim.Wait() })
	writePidFile(t, filepath.Join(root, orphanID, shimPidFileName), victim.Process.Pid)
	// vmm.pid points at a PID that is definitely gone: use os.Getpid()+huge offset
	// via a spawned-then-waited process.
	deadVmm := exec.Command("true")
	require.NoError(t, deadVmm.Run())
	writePidFile(t, filepath.Join(root, orphanID, vmmPidFileName), deadVmm.Process.Pid)

	origRoot := cubeletBundleRoot
	cubeletBundleRoot = root
	t.Cleanup(func() { cubeletBundleRoot = origRoot })

	knownSb := newCubeboxWithStatusForTest(knownID, cubeboxstore.Status{Pid: 0})
	l := &local{cubeboxManger: &fakeCubeboxAPI{cb: knownSb}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.reapStartupOrphanShims(ctx)

	assert.True(t, utils.ProcessAlive(sleeper.Process.Pid), "known-sandbox shim must be left alone")
	assert.False(t, utils.ProcessAlive(victim.Process.Pid), "orphan shim must be reaped")
}

func TestReapStartupOrphanShimsRespectsEnvDisable(t *testing.T) {
	root := t.TempDir()
	id := "sb-should-not-touch"
	require.NoError(t, os.MkdirAll(filepath.Join(root, id), 0o755))
	survivor := exec.Command("sleep", "10")
	require.NoError(t, survivor.Start())
	t.Cleanup(func() { _ = survivor.Process.Kill(); _ = survivor.Wait() })
	writePidFile(t, filepath.Join(root, id, shimPidFileName), survivor.Process.Pid)
	deadVmm := exec.Command("true")
	require.NoError(t, deadVmm.Run())
	writePidFile(t, filepath.Join(root, id, vmmPidFileName), deadVmm.Process.Pid)

	origRoot := cubeletBundleRoot
	cubeletBundleRoot = root
	t.Cleanup(func() { cubeletBundleRoot = origRoot })
	t.Setenv("CUBELET_STARTUP_ORPHAN_REAP", "false")

	l := &local{cubeboxManger: &fakeCubeboxAPI{}}
	l.reapStartupOrphanShims(context.Background())

	assert.True(t, utils.ProcessAlive(survivor.Process.Pid), "env disable must prevent any reap")
}
