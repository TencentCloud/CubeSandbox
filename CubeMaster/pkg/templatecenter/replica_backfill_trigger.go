// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

// New-node backfill trigger.
//
// The replica backfill sweep runs inside the image-job reconcile pass, once
// every imageJobReconcileInterval (5 minutes). A node that joins the cluster
// (scale-out, a replaced or re-imaged host) cannot serve any existing template
// until it holds a replica, so it stays unusable for up to a whole interval
// after it turned healthy. The trigger watches the master's in-memory node
// cache, which is cheap to read, and as soon as a node it has not covered yet
// becomes healthy and schedulable it runs a backfill pass limited to that
// node.
//
// A node keeps its state while it is out of the healthy/schedulable set for
// less than one periodic interval: a node that flaps neither triggers a new
// pass nor gets a fresh retry budget each time it comes back. One that is away
// longer is forgotten, and backfilled again when it returns.
//
// A pass that could not push everything (the new cubelet may still be
// settling) is retried on a short interval for a bounded number of attempts;
// after that the node is left to the periodic sweep. Only distribution
// failures count: a failed AppSnapshot prepare is logged by the pass and
// retried by the periodic sweep, like any other replica that is not READY.
// The trigger only speeds coverage up; the periodic sweep, which reads the
// same bounded batch of templates, remains what guarantees it.
//
// The pass takes the same advisory lock as the periodic sweep, so only one
// master replica pushes at a time. Losing the lock to another replica, or
// failing to reach the database to take it, is not an attempt: the node stays
// pending and is tried again later.

const (
	backfillNodeWatchInterval = 10 * time.Second
	backfillNodeRetryInterval = 30 * time.Second
	backfillNodeMaxAttempts   = 10
	// backfillNodeForgetAfter: how long a node must be gone before its
	// covered/pending state is dropped. Shorter absences are flaps; anything
	// longer has been through a periodic sweep in between anyway.
	backfillNodeForgetAfter = imageJobReconcileInterval
)

// Seams for tests.
var (
	backfillWatchNodeIDs = func() []string {
		nodes := localcache.GetHealthyNodes(-1)
		ids := make([]string, 0, nodes.Len())
		for i := range nodes {
			if n := nodes[i]; n != nil && n.SchedulingAllowed() && n.ID() != "" {
				ids = append(ids, n.ID())
			}
		}
		return ids
	}
	runTriggeredBackfill = func(ctx context.Context, nodeIDs map[string]struct{}) (ran bool, failed int, err error) {
		ran, err = withImageJobReconcileLockHeld(ctx, func() error {
			var sweepErr error
			failed, sweepErr = sweepTemplateReplicaBackfill(ctx, nodeIDs)
			return sweepErr
		})
		return ran, failed, err
	}
)

type backfillNodeTrigger struct {
	baselined bool
	// covered: nodes a pass already ran for (or that exhausted retries).
	covered map[string]struct{}
	// pending: nodes awaiting a successful pass, with attempts so far.
	pending map[string]int
	// lastSeen: when each covered or pending node was last observed.
	lastSeen map[string]time.Time
	nextTry  time.Time
}

func newBackfillNodeTrigger() *backfillNodeTrigger {
	return &backfillNodeTrigger{
		covered:  map[string]struct{}{},
		pending:  map[string]int{},
		lastSeen: map[string]time.Time{},
	}
}

// step observes the node cache once and runs a pass when a node needs one.
func (t *backfillNodeTrigger) step(ctx context.Context, now time.Time) {
	current := map[string]struct{}{}
	for _, id := range backfillWatchNodeIDs() {
		current[id] = struct{}{}
	}
	if !t.baselined {
		// The reconciler runs a full pass at startup; nodes present then are
		// already covered by it.
		t.baselined = true
		for id := range current {
			t.covered[id] = struct{}{}
			t.lastSeen[id] = now
		}
		return
	}
	for id, seen := range t.lastSeen {
		if _, ok := current[id]; ok || now.Sub(seen) < backfillNodeForgetAfter {
			continue
		}
		delete(t.lastSeen, id)
		delete(t.covered, id)
		delete(t.pending, id)
	}
	for id := range current {
		t.lastSeen[id] = now
		if _, ok := t.covered[id]; ok {
			continue
		}
		if _, ok := t.pending[id]; !ok {
			t.pending[id] = 0
			// A node that just arrived must not wait out a retry backoff
			// left by an earlier node.
			t.nextTry = time.Time{}
		}
	}
	// Only nodes that are here now; an absent pending node waits.
	targets := map[string]struct{}{}
	for id := range t.pending {
		if _, ok := current[id]; ok {
			targets[id] = struct{}{}
		}
	}
	if len(targets) == 0 || now.Before(t.nextTry) {
		return
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "image_job_reconcile"})
	ran, failed, err := runTriggeredBackfill(ctx, targets)
	if !ran {
		if err != nil {
			// The lock could not even be tried (database unreachable). Not
			// the node's doing: back off without spending an attempt.
			t.nextTry = now.Add(backfillNodeRetryInterval)
			logger.Warnf("new-node template backfill could not take the reconcile lock: %v; retrying in %s", err, backfillNodeRetryInterval)
		}
		// Otherwise another replica holds the lock; check again next interval.
		return
	}
	if err == nil && failed == 0 {
		// Covered means no push to these nodes failed. It does not mean every
		// template is prepared on them: a failed AppSnapshot prepare, or a
		// template that is broken for every node, is logged by the pass and
		// left to the periodic sweep.
		logger.Infof("new-node template backfill distributed to %d node(s)", len(targets))
		for id := range targets {
			t.covered[id] = struct{}{}
			delete(t.pending, id)
		}
		return
	}
	for id := range targets {
		t.pending[id]++
		if t.pending[id] >= backfillNodeMaxAttempts {
			logger.Warnf("new-node template backfill for %s still failing after %d attempts; leaving it to the periodic sweep", id, t.pending[id])
			t.covered[id] = struct{}{}
			delete(t.pending, id)
		}
	}
	t.nextTry = now.Add(backfillNodeRetryInterval)
	logger.Warnf("new-node template backfill incomplete: failed_templates=%d err=%v; retrying in %s", failed, err, backfillNodeRetryInterval)
}

// startBackfillNodeTrigger runs the trigger until ctx is done.
func startBackfillNodeTrigger(ctx context.Context) {
	go func() {
		trigger := newBackfillNodeTrigger()
		ticker := time.NewTicker(backfillNodeWatchInterval)
		defer ticker.Stop()
		for {
			if isReady() {
				trigger.step(detachTemplateImageJobContext(ctx, "image_job_reconcile", nil), time.Now())
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
