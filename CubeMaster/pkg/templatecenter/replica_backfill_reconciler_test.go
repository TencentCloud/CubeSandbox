// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func testNode(id, ip string) *node.Node { return &node.Node{InsID: id, IP: ip} }

// The selection must skip nodes with a READY clean replica (matched by node
// id OR ip), and re-target nodes whose replica is failed or still
// cleanup_required (half-cleaned by an interrupted delete).
func TestNodesWithoutReadyReplica(t *testing.T) {
	replicas := []models.TemplateReplica{
		{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		{NodeID: "node-c", NodeIP: "10.0.0.3", Status: ReplicaStatusReady, CleanupRequired: true},
	}
	targets := []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
		testNode("node-c", "10.0.0.3"),
		testNode("node-new", "10.0.0.4"),
		nil, // tolerated
	}
	missing := nodesWithoutReadyReplica(replicas, targets)
	got := make([]string, 0, len(missing))
	for _, n := range missing {
		got = append(got, n.ID())
	}
	want := []string{"node-b", "node-c", "node-new"}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing = %v, want %v", got, want)
		}
	}
}

// A replica row keyed by IP only must still count as covering the node.
func TestNodesWithoutReadyReplicaIPOnlyRow(t *testing.T) {
	replicas := []models.TemplateReplica{{NodeIP: "10.0.0.1", Status: ReplicaStatusReady}}
	targets := []*node.Node{testNode("node-a", "10.0.0.1")}
	if missing := nodesWithoutReadyReplica(replicas, targets); len(missing) != 0 {
		t.Fatalf("missing = %v, want none (IP-only replica covers the node)", missing)
	}
}

// The jfs sweep computes its push set from artifact placements, mirroring
// the replica matching: a placement row covers a node by node id OR ip.
// Rows are expected to be pre-filtered by artifact_id by the caller
// (listArtifactNodePlacements); the helper only matches node identity.
func TestNodesWithoutArtifactPlacement(t *testing.T) {
	placements := []models.ArtifactNodePlacement{
		{ArtifactID: "rfs-1", NodeID: "node-a", NodeIP: "10.0.0.1", InstanceType: "test"},
		{ArtifactID: "rfs-1", NodeID: "", NodeIP: "10.0.0.2", InstanceType: "test"},
	}
	targets := []*node.Node{
		testNode("node-a", "10.0.0.1"), // covered by id+ip
		testNode("node-b", "10.0.0.2"), // covered by IP-only row
		testNode("node-new", "10.0.0.4"),
		nil, // tolerated
	}
	missing := nodesWithoutArtifactPlacement(placements, targets)
	if len(missing) != 1 || missing[0].ID() != "node-new" {
		got := make([]string, 0, len(missing))
		for _, n := range missing {
			got = append(got, n.ID())
		}
		t.Fatalf("missing = %v, want [node-new]", got)
	}
}

// stubBackfillSeams swaps every sweep seam for the test and restores them on
// cleanup. The returned distribute recorder captures the push arguments.
func stubBackfillSeams(t *testing.T, job *models.TemplateImageJob, artifact *models.RootfsArtifact, nodes []*node.Node, placements []models.ArtifactNodePlacement) *backfillDistributeRecorder {
	t.Helper()
	recorder := &backfillDistributeRecorder{}
	origJob, origArtifact, origResolve, origPlacements, origDistribute :=
		backfillLatestJob, backfillArtifact, backfillResolveNodes, backfillPlacements, backfillDistribute
	t.Cleanup(func() {
		backfillLatestJob, backfillArtifact, backfillResolveNodes, backfillPlacements, backfillDistribute =
			origJob, origArtifact, origResolve, origPlacements, origDistribute
	})
	backfillLatestJob = func(context.Context, string) (*models.TemplateImageJob, error) { return job, nil }
	backfillArtifact = func(context.Context, string) (*models.RootfsArtifact, error) { return artifact, nil }
	backfillResolveNodes = func(string, []string) ([]*node.Node, error) { return nodes, nil }
	backfillPlacements = func(context.Context, string) ([]models.ArtifactNodePlacement, error) {
		return placements, nil
	}
	backfillDistribute = func(ctx context.Context, req *sandboxtypes.CreateTemplateFromImageReq, generatedReq *sandboxtypes.CreateCubeSandboxReq, art *models.RootfsArtifact, templateID, jobID string, targets []*node.Node, trackReplicas bool) ([]*node.Node, int32, int32, int32, error) {
		recorder.calls = append(recorder.calls, backfillDistributeCall{
			targets:       targets,
			trackReplicas: trackReplicas,
			generatedReq:  generatedReq,
		})
		return targets, int32(len(targets)), int32(len(targets)), 0, nil
	}
	return recorder
}

type backfillDistributeCall struct {
	targets       []*node.Node
	trackReplicas bool
	generatedReq  *sandboxtypes.CreateCubeSandboxReq
}

type backfillDistributeRecorder struct {
	calls []backfillDistributeCall
}

// A READY jfs template is no longer skipped by the sweep: nodes that joined
// or recovered after create lack an artifact placement and get the ext4
// pushed, without fabricating replica rows or needing a generated request.
func TestBackfillTemplateReplicasJFSPushesToPlacementGaps(t *testing.T) {
	stubRemoteTier(t, true, servableProbe(http.StatusPartialContent, "rfs-jfs"))
	raw, err := json.Marshal(&sandboxtypes.CreateTemplateFromImageReq{Backend: "jfs", InstanceType: "cubebox", SourceImageRef: "docker.io/library/nginx:latest", WritableLayerSize: "4Gi"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	job := &models.TemplateImageJob{JobID: "job-1", RequestJSON: string(raw)}
	artifact := &models.RootfsArtifact{
		ArtifactID:    "rfs-jfs",
		Status:        ArtifactStatusReady,
		Ext4SHA256:    "deadbeef",
		Ext4SizeBytes: 1024,
		DownloadToken: "tok",
		MasterNodeIP:  "http://cube-master:8089",
	}
	nodes := []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
		testNode("node-new", "10.0.0.4"),
	}
	placements := []models.ArtifactNodePlacement{
		{ArtifactID: "rfs-jfs", NodeID: "node-a", NodeIP: "10.0.0.1", InstanceType: "test"},
		{ArtifactID: "rfs-jfs", NodeID: "node-b", NodeIP: "10.0.0.2", InstanceType: "test"},
	}
	recorder := stubBackfillSeams(t, job, artifact, nodes, placements)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: "rfs-jfs"}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("distribute calls = %d, want 1", len(recorder.calls))
	}
	call := recorder.calls[0]
	if len(call.targets) != 1 || call.targets[0].ID() != "node-new" {
		got := make([]string, 0, len(call.targets))
		for _, n := range call.targets {
			got = append(got, n.ID())
		}
		t.Fatalf("push targets = %v, want [node-new]", got)
	}
	if call.trackReplicas {
		t.Fatal("jfs artifact push must not write replica rows")
	}
	if call.generatedReq != nil {
		t.Fatal("jfs artifact push must not require a generated request snapshot")
	}
}

// With a placement for every in-scope node the sweep must not push again:
// CreateImage may be idempotent, but a per-tick RPC for every template on
// every node is pure waste.
func TestBackfillTemplateReplicasJFSSkipsFullyPlaced(t *testing.T) {
	stubRemoteTier(t, true, servableProbe(http.StatusPartialContent, "rfs-jfs"))
	raw, err := json.Marshal(&sandboxtypes.CreateTemplateFromImageReq{Backend: "jfs", InstanceType: "cubebox", SourceImageRef: "docker.io/library/nginx:latest", WritableLayerSize: "4Gi"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	job := &models.TemplateImageJob{JobID: "job-1", RequestJSON: string(raw)}
	artifact := &models.RootfsArtifact{
		ArtifactID:    "rfs-jfs",
		Status:        ArtifactStatusReady,
		Ext4SHA256:    "deadbeef",
		Ext4SizeBytes: 1024,
		DownloadToken: "tok",
		MasterNodeIP:  "http://cube-master:8089",
	}
	nodes := []*node.Node{testNode("node-a", "10.0.0.1")}
	placements := []models.ArtifactNodePlacement{{ArtifactID: "rfs-jfs", NodeID: "node-a", NodeIP: "10.0.0.1", InstanceType: "test"}}
	recorder := stubBackfillSeams(t, job, artifact, nodes, placements)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: "rfs-jfs"}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("distribute calls = %d, want 0 (coverage is complete)", len(recorder.calls))
	}
}

// A redo records the nodes it rebuilt on as its request scope. The sweep must
// keep using the template's create scope, or a node that joins after the redo
// is never backfilled.
func TestBackfillAfterRedoUsesCreateScope(t *testing.T) {
	stubRemoteTier(t, true, servableProbe(http.StatusPartialContent, "rfs-jfs"))
	raw, err := json.Marshal(&sandboxtypes.CreateTemplateFromImageReq{
		Backend: "jfs", InstanceType: "cubebox", SourceImageRef: "docker.io/library/nginx:latest", WritableLayerSize: "4Gi",
		DistributionScope: []string{"node-a"}, // the redo's target list
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID: "rfs-jfs", Status: ArtifactStatusReady, Ext4SHA256: "deadbeef",
		Ext4SizeBytes: 1024, DownloadToken: "tok", MasterNodeIP: "http://cube-master:8089",
	}
	nodes := []*node.Node{testNode("node-a", "10.0.0.1"), testNode("node-new", "10.0.0.4")}
	placements := []models.ArtifactNodePlacement{{ArtifactID: "rfs-jfs", NodeID: "node-a", NodeIP: "10.0.0.1"}}
	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: "rfs-jfs"}

	for _, tc := range []struct {
		name      string
		operation string
		create    []string
		want      []string
	}{
		{name: "redo, unscoped or no create job", operation: JobOperationRedo, want: nil},
		{name: "redo, scoped create", operation: JobOperationRedo, create: []string{"node-a", "node-new"}, want: []string{"node-a", "node-new"}},
		{name: "create keeps its own scope", operation: JobOperationCreate, want: []string{"node-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &models.TemplateImageJob{JobID: "job-1", Operation: tc.operation, RequestJSON: string(raw)}
			stubBackfillSeams(t, job, artifact, nodes, placements)
			origScope, origResolve := backfillCreateScope, backfillResolveNodes
			t.Cleanup(func() { backfillCreateScope, backfillResolveNodes = origScope, origResolve })
			backfillCreateScope = func(context.Context, string) ([]string, error) { return tc.create, nil }
			var gotScope []string
			backfillResolveNodes = func(_ string, scope []string) ([]*node.Node, error) {
				gotScope = scope
				return nodes, nil
			}
			if err := backfillTemplateReplicas(context.Background(), def); err != nil {
				t.Fatalf("backfillTemplateReplicas: %v", err)
			}
			if len(gotScope) != len(tc.want) {
				t.Fatalf("resolve scope = %v, want %v", gotScope, tc.want)
			}
			for i := range tc.want {
				if gotScope[i] != tc.want[i] {
					t.Fatalf("resolve scope = %v, want %v", gotScope, tc.want)
				}
			}
		})
	}
}
