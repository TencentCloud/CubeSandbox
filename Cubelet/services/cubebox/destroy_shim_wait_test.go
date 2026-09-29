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

// withShimBundles stands in a fixed set of bundle directories for the shim
// manager, so evidence can be collected without a live containerd.
func withShimBundles(t *testing.T, bundles ...string) {
	t.Helper()
	previous := resolveShimBundles
	resolveShimBundles = func(*local, context.Context, *cubeboxstore.CubeBox) []string { return bundles }
	t.Cleanup(func() { resolveShimBundles = previous })
}

func writeBundlePidFile(t *testing.T, bundle, name string, pid int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(bundle, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte(strconv.Itoa(pid)), 0o644))
}

// The bundle pid files are written by the shim about itself, so a live pid
// there is a real holder even though the file carries no start time.
func TestCollectEvidenceTrustsLiveBundlePidFile(t *testing.T) {
	live := startSleeper(t)
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, shimPidFileName, live.Pid)
	withShimBundles(t, bundle)

	sb := newCubeboxWithStatusForTest("sb-bundle", cubeboxstore.Status{StartedAt: 1})
	// A recorded endpoint that is provably gone: conclusive, so it adds no
	// blocker of its own and cannot mask the bundle pid below.
	sb.Endpoint = sandboxstore.Endpoint{
		Pid:           1000000000,
		PidStartTime:  42,
		ShimSpawned:   true,
		ShimSpawnedAt: time.Now(),
	}

	ev := (&local{shimIntentTTL: time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.unresolved)
	assert.Equal(t, []int{live.Pid}, ev.pids())
}

// The bundle is also consulted when deciding whether a failed create may be
// rolled back: a live pid there must keep the intent in place.
func TestLiveBundleHolderIsFound(t *testing.T) {
	live := startSleeper(t)
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, vmmPidFileName, live.Pid)
	withShimBundles(t, bundle)

	holder, ok := (&local{}).liveBundleHolder(context.Background(), newCubeboxWithStatusForTest("sb-live-holder", cubeboxstore.Status{}))
	require.True(t, ok, "a live bundle pid must be reported as a holder")
	assert.Equal(t, live.Pid, holder.pid)

	withShimBundles(t, t.TempDir()) // exists but empty
	_, ok = (&local{}).liveBundleHolder(context.Background(), newCubeboxWithStatusForTest("sb-no-holder", cubeboxstore.Status{}))
	assert.False(t, ok, "an empty bundle is not a holder")
}

// A pid recorded without a start time, for a sandbox created since
// ShimSpawned existed, cannot be tied to the shim: adopting the number would
// wait on a stranger and then report a live holder that is not ours.
func TestCollectEvidenceRefusesUnprovableRecordedPid(t *testing.T) {
	live := startSleeper(t)
	sb := newCubeboxWithStatusForTest("sb-bare", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{
		Pid:           uint32(live.Pid),
		ShimSpawned:   true,
		ShimSpawnedAt: time.Now(),
	}

	ev := (&local{shimIntentTTL: time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.identities, "an unprovable pid must not be waited on")
	require.Len(t, ev.unresolved, 1, "it must be reported instead, exactly once")
	assert.Contains(t, ev.unresolved[0].message, holderSourceEndpoint)
	assert.False(t, ev.unresolved[0].ageable(), "a pid we can see is never aged out")

	err := waitSandboxRuntimeGone(context.Background(), "sb-bare", ev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unresolved")
}

// Bare pids from a container status are most likely a duplicate of the
// endpoint pid. They must be de-duplicated against it, not reported.
func TestCollectEvidenceDedupsContainerStatusAgainstEndpoint(t *testing.T) {
	live := startSleeper(t)
	sb := newCubeboxWithStatusForTest("sb-dedup", cubeboxstore.Status{Pid: uint32(live.Pid), StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{Pid: uint32(live.Pid), PidStartTime: live.StartTime, ShimSpawned: true}

	ev := (&local{shimIntentTTL: time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.unresolved)
	assert.Equal(t, []int{live.Pid}, ev.pids())
}

// Records written before ShimSpawned existed never recorded a start time, so
// they keep the old behaviour. Failing them closed would make every sandbox
// created before the upgrade undeletable.
func TestCollectEvidenceKeepsPreUpgradeBarePidBehaviour(t *testing.T) {
	live := startSleeper(t)
	sb := newCubeboxWithStatusForTest("sb-legacy-bare", cubeboxstore.Status{Pid: uint32(live.Pid), StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{Pid: uint32(live.Pid)}

	ev := (&local{shimIntentTTL: time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.unresolved)
	assert.Equal(t, []int{live.Pid}, ev.pids())
}

// The regression the intent TTL exists for: a create that wrote the intent and
// then failed before recording a pid leaves a record that names no process at
// all. Without the TTL it blocks cleanup forever.
func TestStaleShimIntentIsReleasedAfterTTL(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-stale-intent", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Hour)}

	ev := (&local{shimIntentTTL: 10 * time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.unresolved, "an intent older than the TTL that names no process must not block cleanup")
	require.NoError(t, waitSandboxRuntimeGone(context.Background(), "sb-stale-intent", ev))
}

// Inside the TTL window the same record still fails closed: that window is
// exactly the crash-between-spawn-and-pid-write case the flag covers.
func TestFreshShimIntentStillFailsClosed(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-fresh-intent", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Minute)}

	ev := (&local{shimIntentTTL: 10 * time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.NotEmpty(t, ev.unresolved)
	require.Error(t, waitSandboxRuntimeGone(context.Background(), "sb-fresh-intent", ev))
}

// An intent whose age could not be established must never expire: aging it out
// would be guessing on the one field the decision is made from.
func TestShimIntentWithoutTimestampNeverAgesOut(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-undated-intent", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true}

	ev := (&local{shimIntentTTL: time.Nanosecond}).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.NotEmpty(t, ev.unresolved)
	require.Error(t, waitSandboxRuntimeGone(context.Background(), "sb-undated-intent", ev))
}

// ttl 0 is the documented fail-closed setting: the intent never ages out.
func TestShimIntentTTLZeroDisablesAging(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-no-ttl", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Hour)}

	ev := (&local{}).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.NotEmpty(t, ev.unresolved)
	require.Error(t, waitSandboxRuntimeGone(context.Background(), "sb-no-ttl", ev))
}

// The TTL must never release resources a process is known to be holding, even
// when the intent itself is ancient.
func TestStaleIntentIsNotReleasedWhileALiveHolderExists(t *testing.T) {
	live := startSleeper(t)
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, shimPidFileName, live.Pid)
	withShimBundles(t, bundle)

	sb := newCubeboxWithStatusForTest("sb-intent-live", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Hour)}

	ev := (&local{shimIntentTTL: time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.Equal(t, []int{live.Pid}, ev.pids(), "the live holder must still be waited on")
	require.NotEmpty(t, ev.unresolved, "and the intent still blocks cleanup until it is gone")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	require.Error(t, waitSandboxRuntimeGone(ctx, "sb-intent-live", ev))
}

// A blocker that is not the intent blocks the TTL too, even when the intent
// itself is well past its age: we can see that pid, so releasing is a guess.
func TestStaleIntentIsNotReleasedWhileAnotherBlockerRemains(t *testing.T) {
	live := startSleeper(t)
	sb := newCubeboxWithStatusForTest("sb-intent-blocked", cubeboxstore.Status{Pid: uint32(live.Pid), StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Hour)}

	ev := (&local{shimIntentTTL: time.Minute}).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.identities)
	require.Len(t, ev.unresolved, 2, "the unprovable pid and the intent both block cleanup")
	require.Error(t, waitSandboxRuntimeGone(context.Background(), "sb-intent-blocked", ev))
}

func TestUnresolvedMessagesPreserveOrderAndText(t *testing.T) {
	var ev sandboxRuntimeEvidence
	ev.markUnresolved("first %d", 1)
	ev.markStaleIntent(time.Now(), "second %s", "intent")
	assert.Equal(t, []string{"first 1", "second intent"}, ev.unresolvedMessages())
}
