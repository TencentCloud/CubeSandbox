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
	unresolved []string
}

func (e *sandboxRuntimeEvidence) markUnresolved(format string, args ...interface{}) {
	e.unresolved = append(e.unresolved, fmt.Sprintf(format, args...))
}

func (e sandboxRuntimeEvidence) pids() []int {
	pids := make([]int, 0, len(e.identities))
	for _, id := range e.identities {
		pids = append(pids, id.Pid)
	}
	return pids
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

	seen := map[int]struct{}{}
	add := func(id utils.ProcessIdentity) {
		if id.Pid <= 1 || id.Pid == os.Getpid() {
			return
		}
		if _, ok := seen[id.Pid]; ok {
			return
		}
		seen[id.Pid] = struct{}{}
		switch st := id.Status(); st {
		case utils.LivenessGone:
			// Recorded and provably finished — including the case where the
			// pid number now belongs to someone else.
			return
		case utils.LivenessUnknown:
			ev.markUnresolved("pid %d liveness is %s", id.Pid, st)
			return
		}
		ev.identities = append(ev.identities, id)
	}
	// addBare handles pids recorded without a start time (container statuses,
	// bundle pid files, pre-upgrade endpoints). Reading the start time now
	// cannot prove the process is ours, but it is enough to wait for this
	// specific incarnation, and it errs towards waiting rather than releasing.
	addBare := func(pid int) {
		if pid <= 1 || pid == os.Getpid() {
			return
		}
		if _, ok := seen[pid]; ok {
			return
		}
		id, err := utils.ReadProcessIdentity(pid)
		if err != nil {
			if os.IsNotExist(err) {
				return // already exited, nothing to wait for
			}
			ev.markUnresolved("pid %d is unreadable: %v", pid, err)
			return
		}
		add(id)
	}

	if ep := sb.Endpoint; ep.Pid > 1 {
		if ep.PidStartTime != 0 {
			add(utils.ProcessIdentity{Pid: int(ep.Pid), StartTime: ep.PidStartTime})
		} else {
			addBare(int(ep.Pid))
		}
	}
	for _, pid := range recordedSandboxPIDs(sb) {
		addBare(pid)
	}
	for _, bundle := range l.shimBundlePaths(ctx, sb) {
		addBare(readPidFile(filepath.Join(bundle, shimPidFileName)))
		addBare(readPidFile(filepath.Join(bundle, vmmPidFileName)))
	}

	// A shim was started for this sandbox but its pid never made it into the
	// store. The process may have exited cleanly or may still be running, and
	// nothing left on this host can tell the two apart — so we must not treat
	// the resulting empty evidence as proof that it is safe to reclaim.
	if sb.Endpoint.ShimSpawned && sb.Endpoint.Pid <= 1 {
		ev.markUnresolved("a shim was spawned but no pid was recorded")
	}

	sort.Slice(ev.identities, func(i, j int) bool { return ev.identities[i].Pid < ev.identities[j].Pid })
	return ev
}

func recordedSandboxPIDs(sb *cubeboxstore.CubeBox) []int {
	if sb == nil {
		return nil
	}
	var pids []int
	if status := sb.GetStatus(); status != nil {
		pids = append(pids, int(status.Get().Pid))
	}
	pids = append(pids, int(sb.Endpoint.Pid))
	if main := sb.FirstContainer(); main != nil && main.Status != nil {
		pids = append(pids, int(main.Status.Get().Pid))
	}
	for _, ctr := range sb.AllContainers() {
		if ctr == nil || ctr.Status == nil {
			continue
		}
		pids = append(pids, int(ctr.Status.Get().Pid))
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
	if len(ev.unresolved) > 0 {
		log.G(ctx).Errorf("sandbox %s: runtime state unresolved (%s); refuse resource cleanup",
			sandboxID, strings.Join(ev.unresolved, "; "))
		return fmt.Errorf("sandbox %s runtime state unresolved: %s",
			sandboxID, strings.Join(ev.unresolved, "; "))
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
