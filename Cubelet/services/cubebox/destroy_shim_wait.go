// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

// resolveShimBundles resolves the on-disk bundle of each shim belonging to a
// sandbox. It is a variable so tests can stand bundles in without a live shim
// manager; production reads them from the shim manager, which is the only
// authoritative mapping from a sandbox to the directory its shim runs in.
var resolveShimBundles = (*local).shimBundlePaths

const (
	shimPidFileName = "shim.pid"
	vmmPidFileName  = "vmm.pid"

	// replaceGateTimeout bounds how long a resume-replace waits for the
	// previous sandbox's shim to exit. Shorter than destroy's deadline on
	// purpose: this runs inside a user-visible create that will be retried,
	// so failing fast and re-checking on the next attempt beats holding the
	// request open.
	replaceGateTimeout = 30 * time.Second
)

// Sources a recorded pid can come from, reported verbatim so that an operator
// reading a quarantine alert can tell which record to distrust.
const (
	holderSourceBundle   = "the shim bundle"
	holderSourceEndpoint = "the sandbox endpoint record"
)

// unresolvedEvidence is one thing we could not decide about.
//
// intentAt is set only when the blocker is the spawn-intent bookkeeping with no
// process anywhere to point at as its holder. That is the one kind of blocker
// the shim-intent TTL may age out; everything else means we found something we
// could not interpret, and waiting never fixes that.
type unresolvedEvidence struct {
	message  string
	intentAt time.Time
}

func (u unresolvedEvidence) ageable() bool { return !u.intentAt.IsZero() }

// sandboxRuntimeEvidence is everything we know about processes that may still
// hold this sandbox's tap fds, IPs and NVMe paths.
//
// The distinction that matters is between "no process is holding anything"
// and "we could not find out". Both produce an empty identity list, and
// conflating them is what lets the network step hand a still-held IP back to
// the pool. Anything we cannot resolve goes into unresolved, and callers must
// refuse to release resources while it is non-empty.
type sandboxRuntimeEvidence struct {
	identities []utils.ProcessIdentity
	unresolved []unresolvedEvidence
}

func (e *sandboxRuntimeEvidence) markUnresolved(format string, args ...interface{}) {
	e.unresolved = append(e.unresolved, unresolvedEvidence{message: fmt.Sprintf(format, args...)})
}

// markStaleIntent records that the only reason this sandbox cannot be declared
// spent is the spawn-intent record itself: a shim may have been started, but no
// process anywhere on the host can be found for it. startedAt is when that
// intent was written; a zero value means its age is unknown, and the intent
// then never ages out.
func (e *sandboxRuntimeEvidence) markStaleIntent(startedAt time.Time, format string, args ...interface{}) {
	e.unresolved = append(e.unresolved, unresolvedEvidence{
		message:  fmt.Sprintf(format, args...),
		intentAt: startedAt,
	})
}

func (e sandboxRuntimeEvidence) unresolvedMessages() []string {
	msgs := make([]string, 0, len(e.unresolved))
	for _, u := range e.unresolved {
		msgs = append(msgs, u.message)
	}
	return msgs
}

func (e sandboxRuntimeEvidence) pids() []int {
	pids := make([]int, 0, len(e.identities))
	for _, id := range e.identities {
		pids = append(pids, id.Pid)
	}
	return pids
}

// resolveStaleIntents drops spawn-intent blockers once they are older than ttl.
// It is the escape hatch for a record that lost the race between writing the
// intent and recording a pid; the alternative is a sandbox that can never be
// destroyed, holding its tap, IP and volumes for the life of the host.
//
// The TTL is deliberately narrow. It applies only when nothing else in the
// evidence is live and every remaining blocker is an intent — never a pid we
// could not read, and never a live pid we could not identify — so it can never
// release resources that some process is known to be holding.
func (e *sandboxRuntimeEvidence) resolveStaleIntents(ctx context.Context, sandboxID string, ttl time.Duration) {
	if ttl <= 0 || len(e.unresolved) == 0 || len(e.identities) > 0 {
		return
	}
	for _, u := range e.unresolved {
		if !u.ageable() {
			return
		}
	}
	now := time.Now()
	for _, u := range e.unresolved {
		if now.Sub(u.intentAt) < ttl {
			// Still inside the window that covers a crash between the spawn
			// and the pid write. Fail closed a little longer.
			return
		}
	}
	log.G(ctx).Warnf("sandbox %s: shim-spawn intent unresolved for longer than %s with no live process "+
		"found for it (%s); treating the intent as stale and allowing resource cleanup",
		sandboxID, ttl, strings.Join(e.unresolvedMessages(), "; "))
	e.unresolved = nil
}

// collectSandboxRuntimeEvidence snapshots every host process that may still
// hold sandbox resources: recorded task/endpoint pids plus the shim bundle's
// shim.pid / vmm.pid. Call this BEFORE DeleteTask — containerd removes the
// bundle on Delete, and TaskExit only means the task slot is gone, not that
// the shim process has released its fds.
func (l *local) collectSandboxRuntimeEvidence(ctx context.Context, sb *cubeboxstore.CubeBox) sandboxRuntimeEvidence {
	var ev sandboxRuntimeEvidence
	if sb == nil {
		return ev
	}

	// Candidates, strongest provenance first: the first entry seen for a pid is
	// the one that decides it.
	//
	//   - the shim bundle's pid files are written by the shim about itself.
	//     They carry no start time, but a live pid there really is this
	//     sandbox's shim, so they are trusted;
	//   - the recorded endpoint is precise when it carries a start time;
	//   - container and sandbox status records are the weakest claim, because
	//     an older code path stored a bare pid there.
	//
	// Bare pids are only trusted for records written before ShimSpawned existed.
	// Those never had a start time to record, and failing every one of them
	// closed would make every sandbox created before the upgrade undeletable, so
	// they keep the previous "wait for whatever holds this number" behaviour.
	// For everything created since, a bare pid that cannot be tied to the shim
	// is reported instead of waited on: adopting a recycled number waits on a
	// stranger until the deadline and then claims a live holder that does not
	// exist.
	trustBare := !sb.Endpoint.ShimSpawned
	var candidates []runtimePIDCandidate
	for _, bundle := range resolveShimBundles(l, ctx, sb) {
		candidates = append(candidates,
			runtimePIDCandidate{
				pid:     readPidFile(filepath.Join(bundle, shimPidFileName)),
				source:  holderSourceBundle,
				trusted: true,
			},
			runtimePIDCandidate{
				pid:     readPidFile(filepath.Join(bundle, vmmPidFileName)),
				source:  holderSourceBundle,
				trusted: true,
			},
		)
	}
	if ep := sb.Endpoint; ep.Pid > 1 {
		c := runtimePIDCandidate{pid: int(ep.Pid), source: holderSourceEndpoint, trusted: trustBare}
		if ep.PidStartTime != 0 {
			identity := utils.ProcessIdentity{Pid: int(ep.Pid), StartTime: ep.PidStartTime}
			// The record carries a start time, so the verdict is conclusive in
			// either direction and provenance no longer matters.
			c.identity = &identity
			c.trusted = true
		}
		candidates = append(candidates, c)
	}
	for _, rec := range recordedSandboxPIDs(sb) {
		candidates = append(candidates, runtimePIDCandidate{
			pid:     rec.pid,
			source:  rec.source,
			trusted: trustBare,
		})
	}

	seen := map[int]struct{}{}
	for _, c := range candidates {
		if c.pid <= 1 || c.pid == os.Getpid() {
			continue
		}
		if _, ok := seen[c.pid]; ok {
			continue
		}
		seen[c.pid] = struct{}{}

		identity := c.identity
		if identity == nil {
			id, err := utils.ReadProcessIdentity(c.pid)
			if err != nil {
				if os.IsNotExist(err) {
					continue // already exited, nothing to wait for
				}
				ev.markUnresolved("pid %d recorded in %s is unreadable: %v", c.pid, c.source, err)
				continue
			}
			if !c.trusted {
				// A live process we cannot tie to this sandbox. The narrow
				// TTL does not cover it either: we can see it, so releasing
				// the sandbox's resources would hand them to a live holder.
				ev.markUnresolved(
					"pid %d recorded in %s has no start time, so it cannot be shown to be this sandbox's",
					c.pid, c.source)
				continue
			}
			identity = &id
		}

		switch identity.Status() {
		case utils.LivenessGone:
			// Provably finished — including the case where the pid number now
			// belongs to someone else entirely.
		case utils.LivenessUnknown:
			ev.markUnresolved("pid %d recorded in %s has no verifiable identity", c.pid, c.source)
		default:
			ev.identities = append(ev.identities, *identity)
		}
	}

	// A shim was started for this sandbox but its pid never made it into the
	// store. The process may have exited cleanly or may still be running, and
	// nothing left on this host can tell the two apart — so we must not treat
	// the resulting empty evidence as proof that it is safe to reclaim.
	//
	// This is the blocker the intent TTL exists for. It is only reached when no
	// bundle pid file and no recorded pid resolved to a live process either, so
	// aging it out never releases a process we can still see.
	if sb.Endpoint.ShimSpawned && sb.Endpoint.Pid <= 1 {
		ev.markStaleIntent(sb.Endpoint.ShimSpawnedAt, "a shim was spawned but no pid was recorded")
	}

	ev.resolveStaleIntents(ctx, sandboxIdentity(sb), l.shimIntentTTL)

	sort.Slice(ev.identities, func(i, j int) bool { return ev.identities[i].Pid < ev.identities[j].Pid })
	return ev
}

// runtimePIDCandidate is one record claiming a pid belongs to this sandbox.
type runtimePIDCandidate struct {
	pid    int
	source string
	// identity is set when the record itself carried a process start time, in
	// which case the liveness verdict is conclusive without a second read.
	identity *utils.ProcessIdentity
	// trusted means the record's provenance is enough to wait on this pid even
	// when it carries no start time.
	trusted bool
}

func sandboxIdentity(sb *cubeboxstore.CubeBox) string {
	if sb == nil {
		return ""
	}
	if sb.SandboxID != "" {
		return sb.SandboxID
	}
	return sb.ID
}

// recordedPID is a pid some record claims belongs to this sandbox, plus where
// that claim came from.
type recordedPID struct {
	pid    int
	source string
}

func recordedSandboxPIDs(sb *cubeboxstore.CubeBox) []recordedPID {
	if sb == nil {
		return nil
	}
	var pids []recordedPID
	if status := sb.GetStatus(); status != nil {
		pids = append(pids, recordedPID{int(status.Get().Pid), "the sandbox status"})
	}
	pids = append(pids, recordedPID{int(sb.Endpoint.Pid), holderSourceEndpoint})
	if main := sb.FirstContainer(); main != nil && main.Status != nil {
		pids = append(pids, recordedPID{int(main.Status.Get().Pid), "the primary container status"})
	}
	for _, ctr := range sb.AllContainers() {
		if ctr == nil || ctr.Status == nil {
			continue
		}
		pids = append(pids, recordedPID{
			int(ctr.Status.Get().Pid),
			fmt.Sprintf("container %s status", ctr.ID),
		})
	}
	return pids
}

func (l *local) shimBundlePaths(ctx context.Context, sb *cubeboxstore.CubeBox) []string {
	if l == nil || l.shims == nil || sb == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var bundles []string
	for _, id := range sandboxShimLookupIDs(sb) {
		shim, err := l.shims.Get(ctx, id)
		if err != nil || shim == nil {
			continue
		}
		bundle := strings.TrimSpace(shim.Bundle())
		if bundle == "" {
			continue
		}
		if _, ok := seen[bundle]; ok {
			continue
		}
		seen[bundle] = struct{}{}
		bundles = append(bundles, bundle)
	}
	return bundles
}

func sandboxShimLookupIDs(sb *cubeboxstore.CubeBox) []string {
	if sb == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(sb.ID)
	if main := sb.FirstContainer(); main != nil {
		add(main.ID)
	}
	for _, ctr := range sb.AllContainers() {
		if ctr != nil {
			add(ctr.ID)
		}
	}
	return ids
}

func readPidFile(path string) int {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// waitSandboxRuntimeGone blocks until every captured runtime process has
// exited. The following workflow step returns the tap/IP to the pool and
// deletes S3 volumes; doing that while the shim is alive hands a live IP to
// the next sandbox and produces host I/O after delete_lvol.
//
// Unresolved evidence fails the wait. An empty identity list only means
// "nothing to wait for" when we are sure there was nothing to find.
func waitSandboxRuntimeGone(ctx context.Context, sandboxID string, ev sandboxRuntimeEvidence) error {
	if msgs := ev.unresolvedMessages(); len(msgs) > 0 {
		log.G(ctx).Errorf("sandbox %s: runtime state unresolved (%s); refuse resource cleanup",
			sandboxID, strings.Join(msgs, "; "))
		return fmt.Errorf("sandbox %s runtime state unresolved: %s",
			sandboxID, strings.Join(msgs, "; "))
	}
	if len(ev.identities) == 0 {
		return nil
	}
	start := time.Now()
	for _, id := range ev.identities {
		if err := utils.WaitIdentityGone(ctx, id); err != nil {
			log.G(ctx).Errorf("sandbox %s: runtime pid %d still alive after %s; refuse resource cleanup: %v",
				sandboxID, id.Pid, time.Since(start), err)
			return err
		}
	}
	if waited := time.Since(start); waited > 20*time.Millisecond {
		log.G(ctx).Warnf("sandbox %s: waited %s for runtime pids %v to exit before resource cleanup",
			sandboxID, waited.Round(time.Millisecond), ev.pids())
	}
	return nil
}

// waitReplacedSandboxGone blocks until the sandbox being replaced has no
// runtime process left, so that deleting its records cannot strand a shim
// that is still holding network and disk resources.
func (l *local) waitReplacedSandboxGone(ctx context.Context, sb *cubeboxstore.CubeBox) error {
	if sb == nil {
		return nil
	}
	evidence := l.collectSandboxRuntimeEvidence(ctx, sb)
	waitCtx, cancel := context.WithTimeout(ctx, replaceGateTimeout)
	defer cancel()
	return waitSandboxRuntimeGone(waitCtx, sb.SandboxID, evidence)
}
