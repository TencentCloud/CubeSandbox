// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
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

// localWithIntentTTL builds the destroy-path receiver with a shim-intent TTL
// set the way the cubebox-service plugin sets it in production.
func localWithIntentTTL(ttl time.Duration) *local {
	l := &local{}
	l.SetShimIntentTTL(ttl)
	return l
}

// pendingIntentOnlyError reports whether err carries the marker the GC retry
// budget keys on. It mirrors exactly what the budget owner does, so a refactor
// that drops the marker fails here rather than silently re-spending the budget.
func pendingIntentOnlyError(t *testing.T, err error) bool {
	t.Helper()
	var marker interface{ RetryWithoutPenalty() bool }
	if !errors.As(err, &marker) {
		return false
	}
	return marker.RetryWithoutPenalty()
}

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

	ev := localWithIntentTTL(time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
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

	ev := localWithIntentTTL(time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
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

	ev := localWithIntentTTL(time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
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

	ev := localWithIntentTTL(time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.unresolved)
	assert.Equal(t, []int{live.Pid}, ev.pids())
}

// The regression the intent TTL exists for: a create that wrote the intent and
// then failed before recording a pid leaves a record that names no process at
// all. Without the TTL it blocks cleanup forever.
func TestStaleShimIntentIsReleasedAfterTTL(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-stale-intent", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Hour)}

	ev := localWithIntentTTL(10*time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.unresolved, "an intent older than the TTL that names no process must not block cleanup")
	require.NoError(t, waitSandboxRuntimeGone(context.Background(), "sb-stale-intent", ev))
}

// Inside the TTL window the same record still fails closed: that window is
// exactly the crash-between-spawn-and-pid-write case the flag covers.
func TestFreshShimIntentStillFailsClosed(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-fresh-intent", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Minute)}

	ev := localWithIntentTTL(10*time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.NotEmpty(t, ev.unresolved)
	err := waitSandboxRuntimeGone(context.Background(), "sb-fresh-intent", ev)
	require.Error(t, err, "the refusal itself must not change: cleanup still fails closed")
	assert.True(t, pendingIntentOnlyError(t, err),
		"an intent still inside its TTL is the one refusal time alone fixes, so it must not spend the retry budget")
}

// An intent whose age could not be established must never expire: aging it out
// would be guessing on the one field the decision is made from.
func TestShimIntentWithoutTimestampNeverAgesOut(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-undated-intent", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true}

	ev := localWithIntentTTL(time.Nanosecond).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.NotEmpty(t, ev.unresolved)
	err := waitSandboxRuntimeGone(context.Background(), "sb-undated-intent", ev)
	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err),
		"an intent with no timestamp never ages out, so it is a real failure and must spend the budget")
}

// ttl 0 is the documented fail-closed setting: the intent never ages out.
func TestShimIntentTTLZeroDisablesAging(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-no-ttl", cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Hour)}

	ev := (&local{}).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.NotEmpty(t, ev.unresolved)
	err := waitSandboxRuntimeGone(context.Background(), "sb-no-ttl", ev)
	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err),
		"ttl 0 means the intent never ages out, so the failure is not self-healing")
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

	ev := localWithIntentTTL(time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.Equal(t, []int{live.Pid}, ev.pids(), "the live holder must still be waited on")
	require.NotEmpty(t, ev.unresolved, "and the intent still blocks cleanup until it is gone")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := waitSandboxRuntimeGone(ctx, "sb-intent-live", ev)
	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err),
		"a live holder is visible and does not clear with time, so the failure must spend the budget")
}

// A blocker that is not the intent blocks the TTL too, even when the intent
// itself is well past its age: we can see that pid, so releasing is a guess.
func TestStaleIntentIsNotReleasedWhileAnotherBlockerRemains(t *testing.T) {
	live := startSleeper(t)
	sb := newCubeboxWithStatusForTest("sb-intent-blocked", cubeboxstore.Status{Pid: uint32(live.Pid), StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now().Add(-time.Hour)}

	ev := localWithIntentTTL(time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
	assert.Empty(t, ev.identities)
	require.Len(t, ev.unresolved, 2, "the unprovable pid and the intent both block cleanup")
	err := waitSandboxRuntimeGone(context.Background(), "sb-intent-blocked", ev)
	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err),
		"a blocker that is not the intent keeps the refusal a real failure")
}

// The classification is about what the evidence contains, not about the error
// text: a bare "runtime state unresolved" with no intent behind it is a real
// failure even though it reads the same to an operator.
func TestPendingIntentMarkerRequiresIntentOnlyEvidence(t *testing.T) {
	var unreadable sandboxRuntimeEvidence
	unreadable.markUnresolved("pid %d recorded in %s is unreadable", 4242, "the sandbox endpoint record")
	err := waitSandboxRuntimeGone(context.Background(), "sb-unreadable", unreadable)
	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err),
		"an unreadable pid never resolves on its own")

	// The same message shape, but produced by the intent record: this one is
	// the deferral case.
	freshIntent := sandboxRuntimeEvidence{intentTTL: time.Minute}
	freshIntent.markStaleIntent(time.Now(), "a shim was spawned but no pid was recorded")
	err = waitSandboxRuntimeGone(context.Background(), "sb-intent-only", freshIntent)
	require.Error(t, err)
	assert.True(t, pendingIntentOnlyError(t, err))

	// The same record once its TTL has run out is no longer self-healing, and
	// must not be deferred: at that point the intent is exactly the stale
	// bookkeeping an operator has to look at.
	var expired sandboxRuntimeEvidence
	expired.markStaleIntent(time.Now().Add(-time.Hour), "a shim was spawned but no pid was recorded")
	err = waitSandboxRuntimeGone(context.Background(), "sb-intent-expired", expired)
	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err))
}

func TestUnresolvedMessagesPreserveOrderAndText(t *testing.T) {
	var ev sandboxRuntimeEvidence
	ev.markUnresolved("first %d", 1)
	ev.markStaleIntent(time.Now(), "second %s", "intent")
	assert.Equal(t, []string{"first 1", "second intent"}, ev.unresolvedMessages())
}

// shortLivedSleeper starts a process that exits on its own shortly, and returns
// the identity of that incarnation. Tests use it for "the holder the wait was
// about is gone by the time the deadline is up".
func shortLivedSleeper(t *testing.T) utils.ProcessIdentity {
	t.Helper()
	cmd := exec.Command("sleep", "0.3")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	id, err := utils.ReadProcessIdentity(cmd.Process.Pid)
	require.NoError(t, err)
	return id
}

// crashWindowEvidence builds the state this PR targets: an intent that says a
// shim may have been spawned with no pid recorded, next to a live process the
// bundle pid file still names.
func crashWindowEvidence(t *testing.T, sandboxID string, holder utils.ProcessIdentity) sandboxRuntimeEvidence {
	t.Helper()
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, shimPidFileName, holder.Pid)
	withShimBundles(t, bundle)

	sb := newCubeboxWithStatusForTest(sandboxID, cubeboxstore.Status{StartedAt: 1})
	sb.Endpoint = sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now()}

	ev := localWithIntentTTL(10*time.Minute).collectSandboxRuntimeEvidence(context.Background(), sb)
	require.Equal(t, []int{holder.Pid}, ev.pids(), "the bundle pid file is the only trace of the live shim")
	require.NotEmpty(t, ev.unresolved, "and the pid that never reached the store still blocks cleanup")
	return ev
}

// The ordering the deferral depends on: the identity in the snapshot is waited
// out first, and only then is the refusal classified. Without that, a live
// identity masks the intent, the round fails hard, and the crash window above
// spends a cleanup attempt and can be quarantined for a state that clears by
// itself.
func TestWaitSandboxRuntimeGoneDefersOnceTheHolderIsGone(t *testing.T) {
	holder := shortLivedSleeper(t)
	ev := crashWindowEvidence(t, "sb-crash-window", holder)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := waitSandboxRuntimeGone(ctx, "sb-crash-window", ev)

	require.Error(t, err, "the refusal itself must not change: nothing is released while the intent stands")
	assert.True(t, pendingIntentOnlyError(t, err),
		"once the only live process is gone, the fresh intent is all that is left and the round must be deferred")
}

// The fail-closed half of the same rule: a holder that does not exit is not
// something time settles, so the round stays a real failure.
func TestWaitSandboxRuntimeGoneStillFailsWhileTheHolderLives(t *testing.T) {
	holder := startSleeper(t)
	ev := crashWindowEvidence(t, "sb-crash-window-live", holder)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := waitSandboxRuntimeGone(ctx, "sb-crash-window-live", ev)

	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err),
		"a holder we can see is not deferred: the sandbox really is still holding its resources")
}

// The resume gate reaches the same wait with the same evidence, so it has to
// come out the same way: wait the old runtime out, and defer only the intent
// that is left behind.
func TestWaitReplacedSandboxGoneDefersOnceTheOldRuntimeIsGone(t *testing.T) {
	holder := shortLivedSleeper(t)
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, shimPidFileName, holder.Pid)
	withShimBundles(t, bundle)

	sb := pausedForReplace("sb-resume-wait", sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := localWithIntentTTL(10*time.Minute).waitReplacedSandboxGone(ctx, sb)
	require.Error(t, err)
	assert.True(t, pendingIntentOnlyError(t, err))
}

// A gate that let a live old runtime through would hand the replacement a tap
// and IP the previous shim still holds.
func TestWaitReplacedSandboxGoneRefusesWhileTheOldRuntimeLives(t *testing.T) {
	holder := startSleeper(t)
	bundle := t.TempDir()
	writeBundlePidFile(t, bundle, shimPidFileName, holder.Pid)
	withShimBundles(t, bundle)

	sb := pausedForReplace("sb-resume-live", sandboxstore.Endpoint{ShimSpawned: true, ShimSpawnedAt: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := localWithIntentTTL(10*time.Minute).waitReplacedSandboxGone(ctx, sb)
	require.Error(t, err)
	assert.False(t, pendingIntentOnlyError(t, err))
}
