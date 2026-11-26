// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

type triggerHarness struct {
	nodes   []string
	runs    int
	targets []string // node IDs of the last pass, sorted
	ran     bool
	failed  int
	err     error
}

func installTriggerHarness(t *testing.T) *triggerHarness {
	t.Helper()
	h := &triggerHarness{ran: true}
	origNodes, origRun := backfillWatchNodeIDs, runTriggeredBackfill
	backfillWatchNodeIDs = func() []string { return append([]string(nil), h.nodes...) }
	runTriggeredBackfill = func(_ context.Context, nodeIDs map[string]struct{}) (bool, int, error) {
		h.runs++
		h.targets = h.targets[:0]
		for id := range nodeIDs {
			h.targets = append(h.targets, id)
		}
		sort.Strings(h.targets)
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

// A node that leaves the healthy/schedulable set for longer than a periodic
// interval and comes back (recovered, uncordoned, re-imaged) is backfilled
// again; a short flap is not.
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
	if h.runs != 0 {
		t.Fatalf("short flap re-triggered: runs = %d", h.runs)
	}

	h.nodes = []string{"node-a"}
	trigger.step(ctx, now.Add(30*time.Second))
	trigger.step(ctx, now.Add(30*time.Second+backfillNodeForgetAfter))
	h.nodes = []string{"node-a", "node-b"}
	trigger.step(ctx, now.Add(40*time.Second+backfillNodeForgetAfter))
	if h.runs != 1 || !reflect.DeepEqual(h.targets, []string{"node-b"}) {
		t.Fatalf("returning node: runs=%d targets=%v, want 1 pass for node-b", h.runs, h.targets)
	}
}

// A pending node that flaps keeps its attempt count, so the cap still bounds
// it; while it is away it is not part of the pass.
func TestBackfillTriggerFlappingNodeKeepsBudget(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()
	trigger.step(ctx, now)

	h.nodes = []string{"node-new"}
	h.failed = 1
	trigger.step(ctx, now)
	if trigger.pending["node-new"] != 1 {
		t.Fatalf("attempts = %d, want 1", trigger.pending["node-new"])
	}
	h.nodes = nil
	now = now.Add(backfillNodeRetryInterval)
	trigger.step(ctx, now)
	if h.runs != 1 {
		t.Fatalf("absent node was pushed: runs = %d", h.runs)
	}
	h.nodes = []string{"node-new"}
	now = now.Add(backfillNodeWatchInterval)
	trigger.step(ctx, now)
	if h.runs != 2 || trigger.pending["node-new"] != 2 {
		t.Fatalf("flap reset the budget: runs=%d attempts=%d, want 2, 2", h.runs, trigger.pending["node-new"])
	}
}

// A pass is limited to the pending nodes that are present.
func TestBackfillTriggerPassTargetsPendingNodes(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()
	h.nodes = []string{"node-a"}
	trigger.step(ctx, now)

	h.nodes = []string{"node-a", "node-x", "node-y"}
	trigger.step(ctx, now.Add(backfillNodeWatchInterval))
	if h.runs != 1 || !reflect.DeepEqual(h.targets, []string{"node-x", "node-y"}) {
		t.Fatalf("runs=%d targets=%v, want one pass for node-x and node-y", h.runs, h.targets)
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

// Failing to reach the database for the lock is not an attempt either, but it
// backs off instead of retrying on every check.
func TestBackfillTriggerLockErrorBacksOffWithoutAttempt(t *testing.T) {
	h := installTriggerHarness(t)
	ctx, now := context.Background(), time.Unix(1000, 0)
	trigger := newBackfillNodeTrigger()
	trigger.step(ctx, now)

	h.nodes = []string{"node-new"}
	h.ran, h.err = false, errors.New("pin connection: db down")
	trigger.step(ctx, now)
	if trigger.pending["node-new"] != 0 || h.runs != 1 {
		t.Fatalf("lock error counted as attempt: pending=%v runs=%d", trigger.pending, h.runs)
	}
	trigger.step(ctx, now.Add(backfillNodeWatchInterval))
	if h.runs != 1 {
		t.Fatalf("retried inside backoff after lock error: runs = %d", h.runs)
	}
	h.ran, h.err = true, nil
	trigger.step(ctx, now.Add(backfillNodeRetryInterval))
	if h.runs != 2 || len(trigger.pending) != 0 {
		t.Fatalf("not retried after backoff: runs=%d pending=%v", h.runs, trigger.pending)
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
	failed, err := sweepTemplateReplicaBackfill(context.Background(), nil)
	if err != nil || failed != 1 {
		t.Fatalf("failed=%d err=%v, want 1 failure", failed, err)
	}
}

// A node-limited pass counts only failures that happen after it found one of
// its nodes missing the template. A template broken for every node (here: no
// job on record) is left to the periodic sweep and must not keep the new node
// retrying; a failed push to the node is counted.
func TestSweepNodeLimitedCountsOnlyTargetFailures(t *testing.T) {
	stubRemoteTier(t, true, servableProbe(http.StatusPartialContent, "rfs-jfs"))
	raw, err := json.Marshal(&sandboxtypes.CreateTemplateFromImageReq{Backend: "jfs", InstanceType: "cubebox", SourceImageRef: "docker.io/library/nginx:latest", WritableLayerSize: "4Gi"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	job := &models.TemplateImageJob{JobID: "job-1", RequestJSON: string(raw)}
	artifact := &models.RootfsArtifact{
		ArtifactID: "rfs-jfs", Status: ArtifactStatusReady, Ext4SHA256: "deadbeef",
		Ext4SizeBytes: 1024, DownloadToken: "tok", MasterNodeIP: "http://cube-master:8089",
	}
	nodes := []*node.Node{testNode("node-a", "10.0.0.1"), testNode("node-new", "10.0.0.4")}
	placements := []models.ArtifactNodePlacement{{ArtifactID: "rfs-jfs", NodeID: "node-a", NodeIP: "10.0.0.1"}}
	stubBackfillSeams(t, job, artifact, nodes, placements)

	oldDB := store.db
	store.db = &gorm.DB{}
	origList := listBackfillDefinitions
	t.Cleanup(func() {
		store.db = oldDB
		listBackfillDefinitions = origList
	})
	listBackfillDefinitions = func(context.Context, int) ([]backfillDefinition, error) {
		return []backfillDefinition{
			{TemplateID: "tpl-no-job", InstanceType: "cubebox"},
			{TemplateID: "tpl-jfs", InstanceType: "cubebox", RootfsArtifactID: "rfs-jfs"},
		}, nil
	}
	backfillLatestJob = func(_ context.Context, id string) (*models.TemplateImageJob, error) {
		if id == "tpl-no-job" {
			return nil, gorm.ErrRecordNotFound
		}
		return job, nil
	}
	only := map[string]struct{}{"node-new": {}}

	failed, err := sweepTemplateReplicaBackfill(context.Background(), only)
	if err != nil || failed != 0 {
		t.Fatalf("push succeeded: failed=%d err=%v; want 0 (tpl-no-job is not the node's)", failed, err)
	}
	if failed, _ := sweepTemplateReplicaBackfill(context.Background(), nil); failed != 1 {
		t.Fatalf("periodic sweep: failed=%d, want 1 (tpl-no-job)", failed)
	}

	pushErr := errors.New("push to node-new failed")
	backfillDistribute = func(_ context.Context, _ *sandboxtypes.CreateTemplateFromImageReq, _ *sandboxtypes.CreateCubeSandboxReq, _ *models.RootfsArtifact, _, _ string, targets []*node.Node, _ replicaWrite) ([]*node.Node, int32, int32, int32, error) {
		return nil, int32(len(targets)), 0, int32(len(targets)), pushErr
	}
	if failed, _ := sweepTemplateReplicaBackfill(context.Background(), only); failed != 1 {
		t.Fatalf("push failed: failed=%d, want 1", failed)
	}
}

// A node-limited pass keeps only the listed nodes; a template none of them is
// in scope for has no target, which the sweep does not count as a failure.
func TestResolveBackfillTargetsLimitsToNodes(t *testing.T) {
	orig := backfillResolveNodes
	t.Cleanup(func() { backfillResolveNodes = orig })
	backfillResolveNodes = func(string, []string) ([]*node.Node, error) {
		return []*node.Node{testNode("node-a", "10.0.0.1"), testNode("node-b", "10.0.0.2")}, nil
	}

	all, err := resolveBackfillTargets("cubebox", nil, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("unlimited: %d node(s), err=%v; want 2", len(all), err)
	}
	got, err := resolveBackfillTargets("cubebox", nil, map[string]struct{}{"node-b": {}, "node-z": {}})
	if err != nil || len(got) != 1 || got[0].ID() != "node-b" {
		t.Fatalf("limited: %v, err=%v; want node-b", got, err)
	}
	if _, err := resolveBackfillTargets("cubebox", nil, map[string]struct{}{"node-z": {}}); !errors.Is(err, ErrNoTemplateNodes) {
		t.Fatalf("no listed node in scope: err=%v, want ErrNoTemplateNodes", err)
	}
}
