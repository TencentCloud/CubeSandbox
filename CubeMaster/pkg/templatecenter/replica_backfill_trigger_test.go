// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

type triggerHarness struct {
	nodes  []string
	runs   int
	ran    bool
	failed int
	err    error
}

func installTriggerHarness(t *testing.T) *triggerHarness {
	t.Helper()
	h := &triggerHarness{ran: true}
	origNodes, origRun := backfillWatchNodeIDs, runTriggeredBackfill
	backfillWatchNodeIDs = func() []string { return append([]string(nil), h.nodes...) }
	runTriggeredBackfill = func(context.Context) (bool, int, error) {
		h.runs++
		return h.ran, h.failed, h.err
	}
	t.Cleanup(func() { backfillWatchNodeIDs, runTriggeredBackfill = origNodes, origRun })
	return h
}

// Nodes present at startup are covered by the reconciler's startup pass; a
// node that joins later triggers one pass right away and then is quiet.
func TestBackfillTriggerRunsOnceForNewNode(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()

	h.nodes = []string{"node-a"}
	trigger.step(ctx, now)
	trigger.step(ctx, now.Add(10*time.Second))
	if h.runs != 0 {
		t.Fatalf("baseline nodes triggered %d pass(es)", h.runs)
	}

	h.nodes = []string{"node-a", "node-new"}
	trigger.step(ctx, now.Add(20*time.Second))
	if h.runs != 1 {
		t.Fatalf("new node runs = %d, want 1", h.runs)
	}
	trigger.step(ctx, now.Add(30*time.Second))
	if h.runs != 1 {
		t.Fatalf("covered node re-triggered: runs = %d", h.runs)
	}
}

// A node leaving the healthy/schedulable set and returning (recovered,
// uncordoned) is backfilled again.
func TestBackfillTriggerRecoveredNodeRetriggers(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()
	h.nodes = []string{"node-a", "node-b"}
	trigger.step(ctx, now)

	h.nodes = []string{"node-a"}
	trigger.step(ctx, now.Add(10*time.Second))
	h.nodes = []string{"node-a", "node-b"}
	trigger.step(ctx, now.Add(20*time.Second))
	if h.runs != 1 {
		t.Fatalf("recovered node runs = %d, want 1", h.runs)
	}
}

// Failed passes are retried on the short interval, bounded by the attempt
// cap, after which the node is left to the periodic sweep.
func TestBackfillTriggerRetriesFailuresWithBound(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()
	trigger.step(ctx, now)

	h.nodes = []string{"node-new"}
	h.failed = 1
	trigger.step(ctx, now)
	if h.runs != 1 {
		t.Fatalf("runs = %d, want 1", h.runs)
	}
	// Inside the retry backoff: no pass.
	trigger.step(ctx, now.Add(backfillNodeWatchInterval))
	if h.runs != 1 {
		t.Fatalf("retried inside backoff: runs = %d", h.runs)
	}
	for i := 0; i < 2*backfillNodeMaxAttempts; i++ {
		now = now.Add(backfillNodeRetryInterval)
		trigger.step(ctx, now)
	}
	if h.runs != backfillNodeMaxAttempts {
		t.Fatalf("runs = %d, want attempt cap %d", h.runs, backfillNodeMaxAttempts)
	}
	if len(trigger.pending) != 0 {
		t.Fatalf("node still pending after cap: %v", trigger.pending)
	}

	// A sweep error counts as a failed attempt too.
	h.runs, h.failed, h.err = 0, 0, errors.New("scan failed")
	h.nodes = []string{"node-new", "node-other"}
	trigger.step(ctx, now.Add(time.Hour))
	if h.runs != 1 || trigger.pending["node-other"] != 1 {
		t.Fatalf("error not counted: runs=%d pending=%v", h.runs, trigger.pending)
	}
	// Success clears the pending node.
	h.err = nil
	trigger.step(ctx, now.Add(time.Hour+backfillNodeRetryInterval))
	if h.runs != 2 || len(trigger.pending) != 0 {
		t.Fatalf("success did not clear: runs=%d pending=%v", h.runs, trigger.pending)
	}
}

// Losing the advisory lock to another replica is not an attempt: the node
// stays pending and is retried on the next check without backoff.
func TestBackfillTriggerLockContentionKeepsNodePending(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()
	trigger.step(ctx, now)

	h.nodes = []string{"node-new"}
	h.ran = false
	trigger.step(ctx, now)
	if trigger.pending["node-new"] != 0 || h.runs != 1 {
		t.Fatalf("lock contention counted as attempt: pending=%v runs=%d", trigger.pending, h.runs)
	}
	h.ran = true
	trigger.step(ctx, now.Add(backfillNodeWatchInterval))
	if h.runs != 2 || len(trigger.pending) != 0 {
		t.Fatalf("pending node not retried after contention: runs=%d pending=%v", h.runs, trigger.pending)
	}
}

// A node joining while an earlier node is in retry backoff is pushed at
// once instead of waiting out that backoff.
func TestBackfillTriggerNewNodeSkipsBackoff(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()
	trigger.step(ctx, now)

	h.nodes = []string{"node-1"}
	h.failed = 1
	trigger.step(ctx, now)
	h.failed = 0
	h.nodes = []string{"node-1", "node-2"}
	trigger.step(ctx, now.Add(backfillNodeWatchInterval))
	if h.runs != 2 || len(trigger.pending) != 0 {
		t.Fatalf("new node waited on backoff: runs=%d pending=%v", h.runs, trigger.pending)
	}
}

// The sweep reports per-template failures to the trigger, but a template
// with no healthy node of its instance type is not one.
func TestSweepTemplateReplicaBackfillCountsFailures(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	origList, origJob := listBackfillDefinitions, backfillLatestJob
	t.Cleanup(func() {
		store.db = oldDB
		listBackfillDefinitions, backfillLatestJob = origList, origJob
	})
	listBackfillDefinitions = func(context.Context, int) ([]backfillDefinition, error) {
		return []backfillDefinition{{TemplateID: "tpl-bad"}, {TemplateID: "tpl-none"}}, nil
	}
	backfillLatestJob = func(_ context.Context, id string) (*models.TemplateImageJob, error) {
		if id == "tpl-none" {
			return nil, fmt.Errorf("resolve: %w", ErrNoTemplateNodes)
		}
		return nil, errors.New("db down")
	}
	failed, err := sweepTemplateReplicaBackfill(context.Background())
	if err != nil || failed != 1 {
		t.Fatalf("failed=%d err=%v, want 1 failure", failed, err)
	}
}
