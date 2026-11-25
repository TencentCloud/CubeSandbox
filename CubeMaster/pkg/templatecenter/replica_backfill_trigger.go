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
// cache, which is cheap to read, and runs a backfill-only pass as soon as a
// node it has not covered yet becomes healthy and schedulable. A node that drops out of that
// set (NotReady, cordoned) and comes back is covered again.
//
// A pass that could not push everything (the new cubelet may still be
// settling) is retried on a short interval for a bounded number of attempts;
// after that the node is left to the periodic sweep. The pass takes the same
// advisory lock as the periodic sweep, so only one master replica pushes at a
// time; a replica that loses the lock leaves the node pending and tries again
// on its next check.

const (
	backfillNodeWatchInterval = 10 * time.Second
	backfillNodeRetryInterval = 30 * time.Second
	backfillNodeMaxAttempts   = 10
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
	runTriggeredBackfill = func(ctx context.Context) (ran bool, failed int, err error) {
		ran, err = withImageJobReconcileLockHeld(ctx, func() error {
			var sweepErr error
			failed, sweepErr = sweepTemplateReplicaBackfill(ctx)
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
	nextTry time.Time
}

func newBackfillNodeTrigger() *backfillNodeTrigger {
	return &backfillNodeTrigger{covered: map[string]struct{}{}, pending: map[string]int{}}
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
		}
		return
	}
	for id := range t.covered {
		if _, ok := current[id]; !ok {
			delete(t.covered, id)
		}
	}
	for id := range t.pending {
		if _, ok := current[id]; !ok {
			delete(t.pending, id)
		}
	}
	for id := range current {
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
	if len(t.pending) == 0 || now.Before(t.nextTry) {
		return
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "image_job_reconcile"})
	ran, failed, err := runTriggeredBackfill(ctx)
	if !ran && err == nil {
		// Another replica holds the lock; check again next interval.
		return
	}
	if err == nil && failed == 0 {
		logger.Infof("new-node template backfill done for %d node(s)", len(t.pending))
		for id := range t.pending {
			t.covered[id] = struct{}{}
			delete(t.pending, id)
		}
		return
	}
	for id := range t.pending {
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
