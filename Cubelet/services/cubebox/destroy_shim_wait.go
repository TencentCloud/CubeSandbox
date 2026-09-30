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

// pendingIntentOnly reports that the evidence refuses cleanup for exactly one
// reason: a spawn-intent record that has not yet reached its TTL. No process
// was found for the sandbox, every remaining blocker is that intent, and the
// clock is still running on it — so the verdict is "too early to tell" rather
// than "something is holding this", and the refusal is expected to clear on
// its own.
//
// It is deliberately strict:
//
//   - a live identity means we can see a holder, and waiting is not optional.
//     The waiter clears the identities it waited out before it asks, so an
//     identity still present here is one that was never waited on;
//   - a blocker that is not an intent (an unreadable pid, a pid we cannot tie
//     to this sandbox) never resolves with time;
//   - an intent with no recorded start time never ages out;
//   - a TTL of 0 disables aging entirely, so nothing here is self-healing.
//
// Anything else must keep the refusal classified as a real failure and spend
// the caller's retry budget.
func (e sandboxRuntimeEvidence) pendingIntentOnly() bool {
	if len(e.unresolved) == 0 || len(e.identities) > 0 || e.intentTTL <= 0 {
		return false
	}
	now := time.Now()
	for _, u := range e.unresolved {
		if !u.ageable() {
			return false
		}
		if now.Sub(u.intentAt) >= e.intentTTL {
			// Already past the TTL: resolveStaleIntents would have dropped it,
			// so the evidence was not aged. Refusing to defer is the safe
			// answer — it spends the budget instead of retrying forever.
			return false
		}
	}
	return true
}

// shimIntentPendingError is the "still inside the shim-spawn intent TTL"
// refusal. Behaviour is unchanged — cleanup still fails closed and releases
// nothing — but it carries a structural marker that lets a caller that owns a
// retry budget (the GC service) defer the round instead of counting it as a
// failed attempt. It is a marker method rather than a shared type so the
// budget owner does not have to import this package.
type shimIntentPendingError struct {
	sandboxID string
	detail    string
}

func (e *shimIntentPendingError) Error() string {
	return fmt.Sprintf("sandbox %s runtime state unresolved: %s", e.sandboxID, e.detail)
}

// RetryWithoutPenalty marks a failure that is expected to clear on its own and
// must therefore not consume a caller's retry budget or raise an alert.
func (e *shimIntentPendingError) RetryWithoutPenalty() bool { return true }

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
	// intentTTL is the window the spawn-intent record may still be inside,
	// copied from the collecting local so the pending/really-stuck decision
	// travels with the evidence instead of being re-derived by a caller that
	// does not know the setting.
	intentTTL time.Duration
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

// runtimeEvidenceScope says which recorded sources of evidence a caller will
// act on. The two callers ask different questions of the same host state.
type runtimeEvidenceScope int

const (
	// evidenceEveryRecordedSource is the destroy view: every candidate takes
	// part, trusted or not. Destroy must not release a tap, IP or volume while
	// anything might hold it, so a pid it cannot tie to the shim is reported
	// and the sandbox stays quarantined rather than being reclaimed.
	evidenceEveryRecordedSource runtimeEvidenceScope = iota
	// evidenceVerifiableOnly is the resume gate's view: only evidence that can
	// be tied to one incarnation takes part. A pid that carries no start time
	// and did not come from the shim's own bundle files cannot answer the
	// gate's question, and waiting on one waits on whoever holds that number
	// now — a resume master allowed would stall for the gate timeout and then
	// keep failing for as long as the number stays occupied, with nothing to
	// bound it.
	evidenceVerifiableOnly
)

// collectSandboxRuntimeEvidence snapshots every host process that may still
// hold sandbox resources: recorded task/endpoint pids plus the shim bundle's
// shim.pid / vmm.pid. Call this BEFORE DeleteTask — containerd removes the
// bundle on Delete, and TaskExit only means the task slot is gone, not that
// the shim process has released its fds.
func (l *local) collectSandboxRuntimeEvidence(ctx context.Context, sb *cubeboxstore.CubeBox) sandboxRuntimeEvidence {
	return l.collectRuntimeEvidence(ctx, sb, evidenceEveryRecordedSource)
}

// collectVerifiableRuntimeEvidence is the resume gate's view of the same state.
//
// Records written before ShimSpawned existed carry no start time anywhere, so
// an endpoint, status or container pid from one of them cannot be shown to be
// this sandbox's shim: it is a number, and numbers are recycled. The gate drops
// those instead of waiting on them. The drop is logged rather than silent, so
// the fail-open is visible to whoever has to reason about an upgrade-era
// tombstone. The destroy path deliberately keeps trusting them — failing every
// pre-upgrade sandbox closed would make all of them undeletable.
func (l *local) collectVerifiableRuntimeEvidence(ctx context.Context, sb *cubeboxstore.CubeBox) sandboxRuntimeEvidence {
	return l.collectRuntimeEvidence(ctx, sb, evidenceVerifiableOnly)
}

func (l *local) collectRuntimeEvidence(ctx context.Context, sb *cubeboxstore.CubeBox, scope runtimeEvidenceScope) sandboxRuntimeEvidence {
	var ev sandboxRuntimeEvidence
	if sb == nil {
		return ev
	}

	// Candidates, strongest provenance first: for a pid no candidate can settle
	// conclusively, the first entry seen for it is the one that decides it.
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

	// A pid number some candidate can settle conclusively is settled for every
	// candidate that names it. The bundle pid files record the shim's own id and
	// Endpoint.Pid is that same id, so the two are one process rather than two
	// sources — and the bundle entry, which carries no start time, would
	// otherwise win the dedup below. Without this the recorded start time is
	// never consulted on the destroy path, and a number recycled onto an
	// unrelated process is waited on as if it were ours.
	conclusivePids := map[int]struct{}{}
	for _, c := range candidates {
		if c.identity != nil {
			conclusivePids[c.pid] = struct{}{}
		}
	}

	seen := map[int]struct{}{}
	for _, c := range candidates {
		if c.pid <= 1 || c.pid == os.Getpid() {
			continue
		}
		if _, ok := seen[c.pid]; ok {
			continue
		}
		if c.identity == nil {
			if _, conclusive := conclusivePids[c.pid]; conclusive {
				continue
			}
			if scope == evidenceVerifiableOnly && c.source != holderSourceBundle {
				log.G(ctx).Warnf("sandbox %s: ignoring pid %d from %s while gating a resume: it "+
					"carries no start time, so waiting on it would wait on whoever holds that "+
					"number now; the destroy path still refuses to release its resources",
					sandboxIdentity(sb), c.pid, c.source)
				continue
			}
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
	// This is the blocker the intent TTL exists for, and it is recorded
	// whenever the intent shape is present — including when some other source
	// did name a live process for this sandbox. The two are decided separately:
	// the live process is waited out first (see waitSandboxRuntimeGone), and
	// only an intent that is left with nothing alive to hold the sandbox can
	// age out. That separation is what keeps aging from ever releasing a
	// process we can still see.
	if sb.Endpoint.ShimSpawned && sb.Endpoint.Pid <= 1 {
		ev.markStaleIntent(sb.Endpoint.ShimSpawnedAt, "a shim was spawned but no pid was recorded")
	}

	ev.intentTTL = l.ShimIntentTTL()
	ev.resolveStaleIntents(ctx, sandboxIdentity(sb), ev.intentTTL)

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
// Waiting comes first, before any verdict is reported. The evidence is a
// snapshot, and the identities in it are the part time alone can settle, so a
// snapshot that reads "a process may still hold this, and the spawn intent is
// still inside its TTL" has to be judged again once that process is gone: the
// intent is then the only thing left refusing, which is precisely the
// deferrable case the caller's retry budget must not be spent on. An identity
// that does not exit in time is reported as the failure it is.
//
// Unresolved evidence fails the wait. An empty identity list only means
// "nothing to wait for" when we are sure there was nothing to find.
func waitSandboxRuntimeGone(ctx context.Context, sandboxID string, ev sandboxRuntimeEvidence) error {
	if len(ev.identities) > 0 {
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
		// Everything waited on above is provably gone, so none of it can be a
		// reason to refuse any more. What is left is the bookkeeping verdict,
		// and it is judged on its own.
		ev.identities = nil
	}
	if msgs := ev.unresolvedMessages(); len(msgs) > 0 {
		// A refusal whose only cause is a spawn-intent record still inside its
		// TTL is not a stuck sandbox: it is a sandbox that is simply too young
		// to be judged. Report it with the same fail-closed refusal so no
		// resource is released, but tag it so the caller does not spend its
		// retry budget on it — otherwise a cleanup ticker faster than the TTL
		// quarantines every sandbox that ever loses the intent/pid race.
		if ev.pendingIntentOnly() {
			log.G(ctx).Warnf("sandbox %s: still inside the shim-spawn intent TTL (%s); refusing resource cleanup and retrying later",
				sandboxID, strings.Join(msgs, "; "))
			return &shimIntentPendingError{sandboxID: sandboxID, detail: strings.Join(msgs, "; ")}
		}
		log.G(ctx).Errorf("sandbox %s: runtime state unresolved (%s); refuse resource cleanup",
			sandboxID, strings.Join(msgs, "; "))
		return fmt.Errorf("sandbox %s runtime state unresolved: %s",
			sandboxID, strings.Join(msgs, "; "))
	}
	return nil
}

// waitReplacedSandboxGone blocks until the sandbox being replaced has no
// runtime process left, so that deleting its records cannot strand a shim
// that is still holding network and disk resources.
//
// It reads the host through the verifiable-evidence view: a replacement may
// not be gated on a pid number nobody can tie to this sandbox, or a record
// that predates ShimSpawned would be un-resumable for as long as that number
// happened to be occupied.
func (l *local) waitReplacedSandboxGone(ctx context.Context, sb *cubeboxstore.CubeBox) error {
	if sb == nil {
		return nil
	}
	evidence := l.collectVerifiableRuntimeEvidence(ctx, sb)
	waitCtx, cancel := context.WithTimeout(ctx, replaceGateTimeout)
	defer cancel()
	return waitSandboxRuntimeGone(waitCtx, sb.SandboxID, evidence)
}
