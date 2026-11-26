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
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// The replica-backfill sweep must not stop at DISTRIBUTED: after a successful
// CreateImage push to a node without a READY replica, the non-jfs branch must
// run the AppSnapshot step on every ready target and persist the outcome via
// UpsertReplica so the row converges to READY. jfs stays placement-only and
// must never run AppSnapshot.
//
// Every collaborator the sweep touches is a package-level seam (the same
// style as backfillDistribute/backfillReplicas); the tests observe the calls
// through recorders, no gomonkey.

type backfillPrepareFixture struct {
	job        *models.TemplateImageJob
	artifact   *models.RootfsArtifact
	nodes      []*node.Node
	replicas   []models.TemplateReplica
	placements []models.ArtifactNodePlacement

	// distribute behavior
	distFailed int32
	distErr    error

	// createReplicaOnNode (AppSnapshot) behavior: nodeID -> failure message
	snapshotErrByNode map[string]string

	// upsert behavior
	upsertErr error

	// definition recheck behavior: nil = found; RootfsArtifactID on the returned row
	definitionErr        error
	definitionErrAfter   int // >0: return definitionErr only from this call index (0-based)
	definitionArtifactID string
	definitionStatus     string // "" = READY
	// per-call status sequence (last value repeats), simulating a definition
	// demoted between the pre-cleanup and pre-persist rechecks
	definitionStatuses []string
	// per-call RootfsArtifactID sequence (last value repeats), simulating a
	// redo that switches the artifact between the pre-cleanup and pre-persist
	// rechecks
	definitionArtifactIDs []string
	definitionCalls       int

	// when set, backfillReplicas pops one entry per call (simulates rows
	// changing between the pre-lock listing and the post-lock re-list).
	// replicasSeqErr is a parallel per-call error: a non-nil entry makes that
	// listing call fail.
	replicasSeq     [][]models.TemplateReplica
	replicasSeqErr  []error
	replicasSeqCall int

	// resolveNodes behavior
	resolveErr error

	// cleanup behavior
	cleanupErr error

	mu            sync.Mutex
	events        []string // "cleanup" / "snapshot:<node>" / "defcheck" — call order
	distCalls     []backfillDistributeCall
	snapshotCalls []backfillSnapshotCall
	upserts       []backfillUpsertCall
	cleanups      []backfillCleanupCall
}

type backfillSnapshotCall struct {
	nodeID       string
	templateID   string
	instanceType string
	annotationID string
	artifactID   string
	jobID        string
}

type backfillUpsertCall struct {
	templateID   string
	instanceType string
	replica      ReplicaStatus
}

type backfillCleanupCall struct {
	templateID string
	replicas   []models.TemplateReplica
	targets    []*node.Node
}

func (f *backfillPrepareFixture) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

// newBackfillPrepareFixture builds the standard non-jfs (xfs) job/artifact
// pair: a READY servable artifact with a generated request snapshot left by
// the FIRST builder (tpl-other) — the sweep must derive its own per-template
// request and not reuse that snapshot.
func newBackfillPrepareFixture(t *testing.T, backend string) *backfillPrepareFixture {
	t.Helper()
	// Shape matches a real persisted snapshot: marshalTemplateImageJobRequest
	// strips the transport envelope, so decoding must re-materialize it.
	raw, err := json.Marshal(&sandboxtypes.CreateTemplateFromImageReq{
		Backend:           backend,
		InstanceType:      "cubebox",
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
	})
	if err != nil {
		t.Fatalf("marshal job request: %v", err)
	}
	// Simulates an artifact row SHARED with an earlier template: the persisted
	// generated request carries tpl-other's template id and request.
	genRaw, err := json.Marshal(&sandboxtypes.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID: "tpl-other",
		},
	})
	if err != nil {
		t.Fatalf("marshal generated request: %v", err)
	}
	imageCfgRaw, err := json.Marshal(&DockerImageConfig{
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "sleep 1"},
	})
	if err != nil {
		t.Fatalf("marshal image config: %v", err)
	}
	return &backfillPrepareFixture{
		job: &models.TemplateImageJob{JobID: "job-1", RequestJSON: string(raw)},
		artifact: &models.RootfsArtifact{
			ArtifactID:           "rfs-xfs-1",
			Status:               ArtifactStatusReady,
			Ext4SHA256:           "deadbeef",
			Ext4SizeBytes:        1024,
			DownloadToken:        "tok",
			MasterNodeIP:         "http://cube-master:8089",
			ImageConfigJSON:      string(imageCfgRaw),
			WritableLayerSize:    "4Gi",
			GeneratedRequestJSON: string(genRaw),
		},
	}
}

// stub swaps every seam the sweep touches and restores them on cleanup. The
// real generateTemplateCreateRequest still runs, so the tests exercise the
// per-template request derivation (shared artifacts included).
func (f *backfillPrepareFixture) stub(t *testing.T) {
	t.Helper()
	stubRemoteTier(t, true, servableProbe(http.StatusPartialContent, f.artifact.ArtifactID))

	origJob, origArt, origRepl, origResolve, origPlace, origDist :=
		backfillLatestJob, backfillArtifact, backfillReplicas, backfillResolveNodes, backfillPlacements, backfillDistribute
	origCreate, origUpsert, origDef, origCleanup :=
		backfillCreateReplica, backfillUpsertReplica, backfillGetDefinition, backfillCleanupReplicas
	t.Cleanup(func() {
		backfillLatestJob, backfillArtifact, backfillReplicas, backfillResolveNodes, backfillPlacements, backfillDistribute =
			origJob, origArt, origRepl, origResolve, origPlace, origDist
		backfillCreateReplica, backfillUpsertReplica, backfillGetDefinition, backfillCleanupReplicas =
			origCreate, origUpsert, origDef, origCleanup
	})
	backfillLatestJob = func(context.Context, string) (*models.TemplateImageJob, error) { return f.job, nil }
	backfillArtifact = func(context.Context, string) (*models.RootfsArtifact, error) { return f.artifact, nil }
	backfillReplicas = func(context.Context, string) ([]models.TemplateReplica, error) {
		if f.replicasSeq != nil {
			i := f.replicasSeqCall
			f.replicasSeqCall++
			if i < len(f.replicasSeqErr) && f.replicasSeqErr[i] != nil {
				return nil, f.replicasSeqErr[i]
			}
			if i >= len(f.replicasSeq) {
				return nil, nil
			}
			return f.replicasSeq[i], nil
		}
		return f.replicas, nil
	}
	backfillResolveNodes = func(string, []string) ([]*node.Node, error) {
		if f.resolveErr != nil {
			return nil, f.resolveErr
		}
		return f.nodes, nil
	}
	backfillPlacements = func(context.Context, string) ([]models.ArtifactNodePlacement, error) {
		return f.placements, nil
	}
	backfillDistribute = func(ctx context.Context, req *sandboxtypes.CreateTemplateFromImageReq, generatedReq *sandboxtypes.CreateCubeSandboxReq, art *models.RootfsArtifact, templateID, jobID string, targets []*node.Node, write replicaWrite) ([]*node.Node, int32, int32, int32, error) {
		f.mu.Lock()
		f.distCalls = append(f.distCalls, backfillDistributeCall{
			targets:      targets,
			write:        write,
			generatedReq: generatedReq,
		})
		f.mu.Unlock()
		return targets, int32(len(targets)), int32(len(targets)), f.distFailed, f.distErr
	}
	backfillCreateReplica = func(_ context.Context, target *node.Node, templateID string, req *sandboxtypes.CreateCubeSandboxReq, opts replicaRunOptions) (ReplicaStatus, string) {
		f.record("snapshot:" + target.ID())
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snapshotCalls = append(f.snapshotCalls, backfillSnapshotCall{
			nodeID:       target.ID(),
			templateID:   templateID,
			instanceType: req.InstanceType,
			annotationID: req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID],
			artifactID:   opts.ArtifactID,
			jobID:        opts.JobID,
		})
		replica := ReplicaStatus{
			NodeID:       target.ID(),
			NodeIP:       target.HostIP(),
			InstanceType: req.InstanceType,
			ArtifactID:   opts.ArtifactID,
			LastJobID:    opts.JobID,
		}
		if msg := f.snapshotErrByNode[target.ID()]; msg != "" {
			replica.Status = ReplicaStatusFailed
			replica.Phase = ReplicaPhaseFailed
			replica.LastErrorPhase = ReplicaPhaseFailed
			replica.CleanupRequired = true
			replica.ErrorMessage = msg
			return replica, ""
		}
		replica.Status = ReplicaStatusReady
		replica.Phase = ReplicaPhaseReady
		replica.CleanupRequired = false
		return replica, ""
	}
	backfillUpsertReplica = func(_ context.Context, templateID, instanceType string, replica ReplicaStatus) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.upserts = append(f.upserts, backfillUpsertCall{templateID: templateID, instanceType: instanceType, replica: replica})
		return f.upsertErr
	}
	backfillGetDefinition = func(context.Context, string) (*models.TemplateDefinition, error) {
		f.record("defcheck")
		f.mu.Lock()
		call := f.definitionCalls
		f.definitionCalls++
		f.mu.Unlock()
		if f.definitionErr != nil && call >= f.definitionErrAfter {
			return nil, f.definitionErr
		}
		id := f.definitionArtifactID
		if len(f.definitionArtifactIDs) > 0 {
			i := call
			if i >= len(f.definitionArtifactIDs) {
				i = len(f.definitionArtifactIDs) - 1
			}
			id = f.definitionArtifactIDs[i]
		}
		status := f.definitionStatus
		if len(f.definitionStatuses) > 0 {
			i := call
			if i >= len(f.definitionStatuses) {
				i = len(f.definitionStatuses) - 1
			}
			status = f.definitionStatuses[i]
		}
		if status == "" {
			status = StatusReady
		}
		return &models.TemplateDefinition{RootfsArtifactID: id, Status: status}, nil
	}
	backfillCleanupReplicas = func(_ context.Context, templateID string, replicas []models.TemplateReplica, targets []*node.Node, backend string) error {
		f.record("cleanup")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cleanups = append(f.cleanups, backfillCleanupCall{templateID: templateID, replicas: replicas, targets: targets})
		return f.cleanupErr
	}
}

func (f *backfillPrepareFixture) eventsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *backfillPrepareFixture) upsertsSnapshot() []backfillUpsertCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]backfillUpsertCall(nil), f.upserts...)
}

// Normal path: a node missing its replica gets CreateImage (distribute), then
// the AppSnapshot prepare step, and the persisted row is READY/READY.
func TestBackfillPrepareReplicaReadyAfterDistribute(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 1 || f.distCalls[0].write != replicaWriteRepair {
		t.Fatalf("distribute calls = %+v, want 1 repair write call", f.distCalls)
	}
	if len(f.snapshotCalls) != 1 {
		t.Fatalf("AppSnapshot calls = %d, want 1 (the sweep must prepare the replica after CreateImage)", len(f.snapshotCalls))
	}
	call := f.snapshotCalls[0]
	if call.nodeID != "node-a" || call.templateID != "tpl-1" || call.instanceType != "cubebox" ||
		call.artifactID != f.artifact.ArtifactID || call.jobID != "job-1" {
		t.Fatalf("AppSnapshot call = %+v, want node-a/tpl-1/cubebox/rfs-xfs-1/job-1", call)
	}
	if call.annotationID != "tpl-1" {
		t.Fatalf("AppSnapshot annotation template id = %q, want tpl-1 (derived from THIS template, not the artifact snapshot)", call.annotationID)
	}
	if len(f.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(f.upserts))
	}
	up := f.upserts[0]
	if up.templateID != "tpl-1" || up.instanceType != "cubebox" {
		t.Fatalf("upsert keys = %q/%q, want tpl-1/cubebox", up.templateID, up.instanceType)
	}
	r := up.replica
	if r.NodeID != "node-a" || r.Status != ReplicaStatusReady || r.Phase != ReplicaPhaseReady {
		t.Fatalf("replica = %+v, want node-a Status=READY Phase=READY", r)
	}
	if r.CleanupRequired {
		t.Fatalf("a READY replica must not be cleanup_required: %+v", r)
	}
	if r.ErrorMessage != "" {
		t.Fatalf("a READY replica must carry no error message: %+v", r)
	}
}

// A SHARED artifact (identical fingerprint, first builder tpl-other) must
// still prepare the sweep's OWN template: the AppSnapshot annotation and the
// distribute/upsert keys all carry def.TemplateID, never the artifact
// snapshot's. This is the data-corruption case from review: reusing
// GeneratedRequestJSON marked T2 READY while recreating T1 on disk.
func TestBackfillSharedArtifactUsesOwnTemplateID(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-2", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("distribute calls = %d, want 1", len(f.distCalls))
	}
	if got := f.distCalls[0].generatedReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]; got != "tpl-2" {
		t.Fatalf("distribute generated request annotation = %q, want tpl-2", got)
	}
	if len(f.snapshotCalls) != 1 {
		t.Fatalf("AppSnapshot calls = %d, want 1", len(f.snapshotCalls))
	}
	if got := f.snapshotCalls[0].annotationID; got != "tpl-2" {
		t.Fatalf("AppSnapshot annotation = %q, want tpl-2 (the artifact snapshot says tpl-other)", got)
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 1 || upserts[0].templateID != "tpl-2" {
		t.Fatalf("upserts = %+v, want one row keyed tpl-2", upserts)
	}
}

// AppSnapshot failure: the FAILED outcome is persisted with an error message,
// is not fatal to the sweep, and the node stays in the missing set so the
// next tick retries it.
func TestBackfillPrepareReplicaFailureRecordedAndRetried(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.snapshotErrByNode = map[string]string{"node-a": "appsnapshot exploded"}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("a failed AppSnapshot must be recorded, not returned as a sweep error: %v", err)
	}
	if len(f.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(f.upserts))
	}
	r := f.upserts[0].replica
	if r.Status != ReplicaStatusFailed || r.ErrorMessage == "" {
		t.Fatalf("failed prepare must upsert Status=FAILED with an error message, got %+v", r)
	}

	// Next sweep: the FAILED row (as replicaNeedsRedo sees it) leaves the node
	// in the missing set, so the push AND the prepare run again.
	f.replicas = []models.TemplateReplica{{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusFailed, Phase: ReplicaPhaseFailed}}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if len(f.distCalls) != 2 {
		t.Fatalf("distribute calls = %d, want 2 (failed node retried next sweep)", len(f.distCalls))
	}
	if got := f.distCalls[1].targets; len(got) != 1 || got[0].ID() != "node-a" {
		t.Fatalf("retry targets = %v, want [node-a]", got)
	}
	if len(f.snapshotCalls) != 2 {
		t.Fatalf("AppSnapshot calls = %d, want 2 (retried node is prepared again)", len(f.snapshotCalls))
	}
}

// A node holding a READY clean replica is skipped entirely: no CreateImage,
// no AppSnapshot, no upsert. Guards the existing behavior against the fix.
func TestBackfillSkipsNodeWithReadyReplica(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.replicas = []models.TemplateReplica{{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady}}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 0 || len(f.snapshotCalls) != 0 || len(f.upserts) != 0 {
		t.Fatalf("READY replica must skip distribute/appsnapshot/upsert, got dist=%d snap=%d upsert=%d",
			len(f.distCalls), len(f.snapshotCalls), len(f.upserts))
	}
}

// A replica stuck at Status=FAILED Phase=DISTRIBUTED (CreateImage succeeded,
// AppSnapshot never ran) counts as missing, so the sweep re-pushes AND
// prepares it — and this time the row must converge to READY instead of
// looping back to DISTRIBUTED.
func TestBackfillRedoesStaleDistributedReplica(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.replicas = []models.TemplateReplica{{
		NodeID: "node-a", NodeIP: "10.0.0.1",
		Status: ReplicaStatusFailed, Phase: ReplicaPhaseDistributed,
	}}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("stale DISTRIBUTED replica must be re-pushed, distribute calls = %d", len(f.distCalls))
	}
	if len(f.snapshotCalls) != 1 {
		t.Fatalf("stale DISTRIBUTED replica must get the AppSnapshot prepare step, calls = %d", len(f.snapshotCalls))
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 1 || upserts[0].replica.Status != ReplicaStatusReady || upserts[0].replica.Phase != ReplicaPhaseReady {
		t.Fatalf("upserts = %+v, want one READY/READY row (converges instead of looping)", upserts)
	}

	// And the loop is actually broken: with the READY row in place the next
	// sweep pushes nothing.
	f.replicas = []models.TemplateReplica{{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady}}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("converged sweep: %v", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("after convergence distribute calls = %d, want still 1 (no redistribute loop)", len(f.distCalls))
	}
}

// Leftover CoW state (a FAILED / cleanup_required row means volumes are still
// on the node, and cubelet's AppSnapshot refuses AlreadyExists forever): the
// sweep must run the redo-style cleanup BEFORE the prepare, and hand the
// stale rows to it.
func TestBackfillCleanupStaleReplicasBeforePrepare(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	stale := models.TemplateReplica{
		NodeID: "node-a", NodeIP: "10.0.0.1",
		Status: ReplicaStatusFailed, Phase: ReplicaPhaseFailed, CleanupRequired: true,
	}
	f.replicas = []models.TemplateReplica{stale}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	events := f.eventsSnapshot()
	// defcheck (pre-cleanup artifact guard) -> cleanup -> prepare.
	if len(events) < 3 || events[0] != "defcheck" || events[1] != "cleanup" || events[2] != "snapshot:node-a" {
		t.Fatalf("call order = %v, want defcheck before cleanup before snapshot:node-a", events)
	}
	if len(f.cleanups) != 1 {
		t.Fatalf("cleanups = %d, want 1", len(f.cleanups))
	}
	cl := f.cleanups[0]
	if cl.templateID != "tpl-1" || len(cl.replicas) != 1 || cl.replicas[0].NodeID != "node-a" {
		t.Fatalf("cleanup call = %+v, want tpl-1 with the stale node-a row", cl)
	}
	if len(f.snapshotCalls) != 1 {
		t.Fatalf("AppSnapshot calls = %d, want 1 (prepare still runs after cleanup)", len(f.snapshotCalls))
	}
}

// A cleanup failure must not block the prepare: the prepare still runs and
// its outcome is persisted. (The cleanup-failure + prepare-failure combo —
// the FAILED row landing despite cleanup failing — is covered by
// TestBackfillPrepareReplicaFailureRecordedAndRetried.)
func TestBackfillCleanupErrorDoesNotBlockPrepare(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.replicas = []models.TemplateReplica{{
		NodeID: "node-a", NodeIP: "10.0.0.1",
		Status: ReplicaStatusFailed, CleanupRequired: true,
	}}
	f.cleanupErr = errors.New("cleanup rpc failed")
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("a cleanup failure must be logged, not returned: %v", err)
	}
	if len(f.snapshotCalls) != 1 {
		t.Fatalf("AppSnapshot calls = %d, want 1 despite cleanup failure", len(f.snapshotCalls))
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 1 || upserts[0].replica.Status != ReplicaStatusReady {
		t.Fatalf("upserts = %+v, want one READY row", upserts)
	}
}

// jfs: placement only — no AppSnapshot prepare, no replica rows.
func TestBackfillJFSNeverRunsAppSnapshot(t *testing.T) {
	f := newBackfillPrepareFixture(t, "jfs")
	f.nodes = []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-new", "10.0.0.4"),
	}
	f.placements = []models.ArtifactNodePlacement{
		{ArtifactID: f.artifact.ArtifactID, NodeID: "node-a", NodeIP: "10.0.0.1", InstanceType: "cubebox"},
	}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 1 || f.distCalls[0].write != replicaWriteNone {
		t.Fatalf("jfs sweep must distribute once without replica writes, got %+v", f.distCalls)
	}
	// The missing set must be derived from the PLACEMENT rows: node-a already
	// holds the ext4 cache, only node-new is pushed. Asserting the targets
	// pins nodesWithoutArtifactPlacement — deriving from replica rows instead
	// would push node-a too and pass every other assertion in this test.
	if got := f.distCalls[0].targets; len(got) != 1 || got[0].ID() != "node-new" {
		t.Fatalf("jfs distribute targets = %v, want [node-new] only (placement-derived missing set)", got)
	}
	if len(f.snapshotCalls) != 0 || len(f.upserts) != 0 {
		t.Fatalf("jfs sweep must never run AppSnapshot or write replica rows, snap=%d upsert=%d",
			len(f.snapshotCalls), len(f.upserts))
	}
}

// Two missing nodes, one AppSnapshot failure: the other node's replica still
// converges to READY, the failure is persisted for the bad node, the sweep
// does not abort, and only the failed node is retried next sweep.
func TestBackfillPartialPrepareFailureDoesNotBlockOtherNodes(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
	}
	f.snapshotErrByNode = map[string]string{"node-b": "node-b appsnapshot failed"}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("one node's AppSnapshot failure must not fail the sweep: %v", err)
	}
	if len(f.snapshotCalls) != 2 {
		t.Fatalf("AppSnapshot calls = %d, want 2 (both ready targets prepared)", len(f.snapshotCalls))
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 2 {
		t.Fatalf("upserts = %d, want 2", len(upserts))
	}
	byNode := map[string]ReplicaStatus{}
	for _, up := range upserts {
		byNode[up.replica.NodeID] = up.replica
	}
	if r := byNode["node-a"]; r.Status != ReplicaStatusReady || r.Phase != ReplicaPhaseReady {
		t.Fatalf("node-a replica = %+v, want READY/READY (failure of node-b must not drag it down)", r)
	}
	if r := byNode["node-b"]; r.Status != ReplicaStatusFailed || r.ErrorMessage == "" {
		t.Fatalf("node-b replica = %+v, want FAILED with error message", r)
	}

	// Next sweep: node-a's READY row (upserted above) is in the listing, so
	// only the failed node-b is retried.
	f.replicas = []models.TemplateReplica{
		{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady},
		{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed, Phase: ReplicaPhaseFailed},
	}
	f.snapshotErrByNode = nil // node-b heals on retry
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if len(f.distCalls) != 2 {
		t.Fatalf("distribute calls = %d, want 2", len(f.distCalls))
	}
	if got := f.distCalls[1].targets; len(got) != 1 || got[0].ID() != "node-b" {
		t.Fatalf("retry targets = %v, want [node-b] only", got)
	}
	upserts = f.upsertsSnapshot()
	if len(upserts) != 3 || upserts[2].replica.Status != ReplicaStatusReady {
		t.Fatalf("retry upserts = %+v, want a third READY row for node-b", upserts)
	}
}

// The prepare step must survive a non-nil distribute error too: whatever
// readyTargets distribute returned still gets prepared, and the distribute
// error is reported (current contract), not masked by prepare failures.
func TestBackfillPrepareRunsDespiteDistributeError(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.distErr = errors.New("node-b create image failed")
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	err := backfillTemplateReplicas(context.Background(), def)
	if err == nil || !errors.Is(err, f.distErr) {
		t.Fatalf("backfillTemplateReplicas err = %v, want it to carry the distribute error", err)
	}
	upserts := f.upsertsSnapshot()
	if len(f.snapshotCalls) != 1 || len(upserts) != 1 || upserts[0].replica.Status != ReplicaStatusReady {
		t.Fatalf("ready targets must be prepared even when distribute reports an error: snap=%d upserts=%+v",
			len(f.snapshotCalls), upserts)
	}
}

// A persist failure must be logged and swallowed (distErr contract
// unchanged), must not register scheduling caches for a row that does not
// exist, and the node is retried next sweep.
func TestBackfillUpsertFailureRecordedAndNotCached(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.upsertErr = errors.New("upsert exploded")
	// Force the registration fallback (fresh re-list fails) so the cache can
	// only contain what `prepared` holds: if an upsert failure ever leaked
	// into prepared, node-a would land in the locality cache here.
	f.replicasSeq = [][]models.TemplateReplica{{}, {}, {}}
	f.replicasSeqErr = []error{nil, nil, errors.New("fresh list fail")}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-upsert", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("an upsert failure must not fail the sweep: %v", err)
	}
	if len(f.snapshotCalls) != 1 || len(f.upserts) != 1 {
		t.Fatalf("snap=%d upserts=%d, want 1/1", len(f.snapshotCalls), len(f.upserts))
	}
	if state := localcache.GetImageStateByNode("tpl-upsert", "node-a"); state != nil {
		t.Fatalf("a failed upsert must not register the node in the locality cache: %+v", state)
	}
}

// After a READY upsert the sweep must publish the node to the scheduling
// caches: the replica registry sees the node and the stale locality/query
// caches are dropped so the next read reloads from the persisted row.
func TestBackfillCacheRefreshedAfterReadyUpsert(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	// The registration step re-reads the replica rows after the prepare;
	// the upserted READY row must be in that listing.
	f.replicasSeq = [][]models.TemplateReplica{
		{}, {},
		{{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady}},
	}
	f.stub(t)

	// Pre-seed a stale locality snapshot as a previous tick left it.
	setTemplateLocalityCache("tpl-cache", []ReplicaStatus{{NodeID: "node-old", NodeIP: "10.0.0.9", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady}})
	t.Cleanup(func() { invalidateTemplateCaches("tpl-cache"); localcache.InvalidateImageState("tpl-cache") })

	def := backfillDefinition{TemplateID: "tpl-cache", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if state := localcache.GetImageStateByNode("tpl-cache", "node-a"); state == nil {
		t.Fatalf("READY upsert must register node-a in the replica locality cache")
	}
	if _, ok := getCachedTemplateLocality("tpl-cache"); ok {
		t.Fatalf("READY upsert must drop the stale locality cache so it reloads from the persisted row")
	}
}

// DeleteTemplate racing an in-flight AppSnapshot: the definition is gone at
// persist time (the pre-cleanup check passed), so the sweep must write NO row
// (the orphan reconciler only scans cleanup_required rows and would never
// reclaim it) and must remove the just-created package from the node.
func TestBackfillDeleteTemplateRaceDropsReplica(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.definitionErr = ErrTemplateNotFound
	f.definitionErrAfter = 1 // pre-cleanup check sees the template; the pre-persist one does not
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 0 {
		t.Fatalf("upserts = %+v, want none when the definition is gone mid-prepare", upserts)
	}
	if len(f.cleanups) != 2 {
		t.Fatalf("cleanups = %d, want 2 (stale sweep + the raced node package)", len(f.cleanups))
	}
	raced := f.cleanups[1]
	if raced.templateID != "tpl-1" || len(raced.replicas) != 1 || raced.replicas[0].NodeID != "node-a" || len(raced.targets) != 1 {
		t.Fatalf("raced cleanup = %+v, want tpl-1 package on node-a only", raced)
	}
}

// A non-READY definition aborts the pass: pushing into a torn-down template
// leaks state.
func TestBackfillSkipsPassWhenDefinitionNotReady(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.definitionStatus = StatusFailed
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-failed", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 0 || len(f.cleanups) != 0 || len(f.snapshotCalls) != 0 || len(f.upserts) != 0 {
		t.Fatalf("a non-READY definition must skip the pass: dist=%d cleanups=%d snap=%d upserts=%d",
			len(f.distCalls), len(f.cleanups), len(f.snapshotCalls), len(f.upserts))
	}
}

// Terminal status mid-prepare: drop the prepared row and reclaim the package.
func TestBackfillDropsPackageWhenDefinitionTurnsTerminalMidPrepare(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.definitionStatuses = []string{StatusReady, StatusFailed}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-torn-down", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if upserts := f.upsertsSnapshot(); len(upserts) != 0 {
		t.Fatalf("upserts = %+v, want none for a terminal definition", upserts)
	}
	if len(f.cleanups) != 2 {
		t.Fatalf("cleanups = %d, want 2 (stale sweep + the package we just built)", len(f.cleanups))
	}
	dropped := f.cleanups[1]
	if dropped.templateID != "tpl-torn-down" || len(dropped.replicas) != 1 || dropped.replicas[0].NodeID != "node-a" {
		t.Fatalf("dropped package = %+v, want tpl-torn-down on node-a only", dropped)
	}
}

// A live-but-partial definition (a redo that ended with mixed replicas) still
// takes the READY outcome: the node holds it, and no row would orphan the package.
func TestBackfillPersistsOutcomeWhenDefinitionTurnsPartiallyReadyMidPrepare(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.definitionStatuses = []string{StatusReady, StatusPartiallyReady}
	f.stub(t)
	t.Cleanup(func() { localcache.InvalidateImageState("tpl-partial") })

	def := backfillDefinition{TemplateID: "tpl-partial", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 1 || upserts[0].replica.Status != ReplicaStatusReady {
		t.Fatalf("upserts = %+v, want the READY outcome persisted", upserts)
	}
}

// A transient GetDefinition error at persist time: no row is written (the
// prepare outcome cannot be attributed safely), and no cache is refreshed.
func TestBackfillDefinitionRecheckErrorSkipsPersist(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.definitionErr = errors.New("db unavailable")
	f.definitionErrAfter = 1 // pre-cleanup check passes; the pre-persist one fails
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-defcheck", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 0 {
		t.Fatalf("upserts = %+v, want none when the definition recheck fails", upserts)
	}
	if state := localcache.GetImageStateByNode("tpl-defcheck", "node-a"); state != nil {
		t.Fatalf("a skipped persist must not register the node in the locality cache: %+v", state)
	}
}

// A definition that already points at a NEW artifact when the pass reaches the
// pre-cleanup check (redo switched mid-pass): skip everything for this tick —
// no push, no cleanup, no prepare, no rows. The next tick loads the new
// artifact. Cleaning from the stale view would delete the fresh package the
// redo just built.
func TestBackfillSkipsPassWhenDefinitionArtifactChanged(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.definitionArtifactIDs = []string{"rfs-new-artifact"}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 0 || len(f.cleanups) != 0 || len(f.snapshotCalls) != 0 || len(f.upserts) != 0 {
		t.Fatalf("a definition that switched artifacts must skip the whole pass: dist=%d cleanups=%d snap=%d upserts=%d",
			len(f.distCalls), len(f.cleanups), len(f.snapshotCalls), len(f.upserts))
	}
}

// A transient error from the pre-cleanup definition check aborts the pass (the
// sweep logs it and retries next tick) instead of cleaning/preparing blind.
func TestBackfillSkipsPassWhenPreCleanupDefinitionCheckFails(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.definitionErr = errors.New("db unavailable")
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err == nil {
		t.Fatalf("backfillTemplateReplicas err = nil, want the definition check error")
	}
	if len(f.distCalls) != 0 || len(f.cleanups) != 0 || len(f.snapshotCalls) != 0 || len(f.upserts) != 0 {
		t.Fatalf("a failing definition check must abort before touching nodes: dist=%d cleanups=%d snap=%d upserts=%d",
			len(f.distCalls), len(f.cleanups), len(f.snapshotCalls), len(f.upserts))
	}
}

// Concurrent prepares must publish ALL freshly READY nodes, not just the
// last goroutine's: per-goroutine invalidate/register stomps each other
// because InvalidateImageState wipes the whole template's registrations.
func TestBackfillCacheRegistersAllReadyNodes(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
	}
	// node-b already holds a READY replica from an earlier pass, so the
	// sweep only backfills node-a. Cache invalidation wipes EVERY node of
	// the template, so the re-register afterwards must cover both —
	// including the node this pass did not touch.
	nodeB := models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady}
	nodeAReady := models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady}
	f.replicasSeq = [][]models.TemplateReplica{
		{nodeB}, {nodeB}, {nodeAReady, nodeB},
	}
	f.stub(t)
	t.Cleanup(func() { invalidateTemplateCaches("tpl-cache2"); localcache.InvalidateImageState("tpl-cache2") })

	def := backfillDefinition{TemplateID: "tpl-cache2", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	for _, nodeID := range []string{"node-a", "node-b"} {
		if state := localcache.GetImageStateByNode("tpl-cache2", nodeID); state == nil {
			t.Fatalf("READY upsert must register %s in the replica locality cache (the FULL ready set, not just the backfilled node)", nodeID)
		}
	}
	if _, ok := getCachedTemplateLocality("tpl-cache2"); ok {
		t.Fatalf("READY upsert must drop the stale locality cache so it reloads from the persisted rows")
	}
}

// Redo switched the definition to a NEW artifact while our prepare ran
// (per-artifact locks do not serialize against a redo producing a different
// artifact). Persisting our stale-artifact outcome would clobber the redone
// row, so nothing is persisted. The node package is deliberately KEPT:
// snapshots are keyed by template id, so dropping it could delete the fresh
// package a concurrent redo just created on this node; the redo's own
// cleanup reclaims the leftover old-artifact state.
func TestBackfillPrepareOutcomeDroppedOnArtifactMismatch(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	// The pre-cleanup check still sees OUR artifact; the redo switches the
	// definition during the prepare, so the pre-persist check sees the new one.
	f.definitionArtifactIDs = []string{f.artifact.ArtifactID, "rfs-new-artifact"}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.snapshotCalls) != 1 {
		t.Fatalf("AppSnapshot calls = %d, want 1 (the prepare itself still runs)", len(f.snapshotCalls))
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 0 {
		t.Fatalf("upserts = %+v, want none when the definition points at another artifact now", upserts)
	}
	if len(f.cleanups) != 1 {
		t.Fatalf("cleanups = %d, want 1 (only the stale sweep batch; the node package must NOT be dropped)", len(f.cleanups))
	}
}

// A node whose replica a concurrent redo converges to READY during the
// (minutes-long) distribute must not be re-prepared: the fresh
// post-distribute listing filters it from readyTargets, avoiding a
// redundant AppSnapshot that would bounce off AlreadyExists.
func TestBackfillPrepareSkipsNodeConvergedDuringDistribute(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.replicasSeq = [][]models.TemplateReplica{
		{},
		{{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady}},
	}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.snapshotCalls) != 0 {
		t.Fatalf("AppSnapshot calls = %d, want 0 (node converged by redo during distribute)", len(f.snapshotCalls))
	}
	if upserts := f.upsertsSnapshot(); len(upserts) != 0 {
		t.Fatalf("upserts = %+v, want none", upserts)
	}
}

// The fixture snapshot has the real persisted shape (no transport
// envelope); this test pins that the sweep decodes it without panicking.
func TestBackfillDecodesJobSnapshotWithoutRequestEnvelope(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas must decode an envelope-less snapshot, got: %v", err)
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 1 || upserts[0].replica.Status != ReplicaStatusReady {
		t.Fatalf("upserts = %+v, want one converged READY row", upserts)
	}
}

// The stale set handed to backfillCleanupReplicas must come from the
// pre-cleanup listing: a row a redo converges to READY during distribute
// must drop out; a row still FAILED stays in. backfillReplicas is read
// three times on this path; the fixture pops one seq entry per call.
func TestBackfillCleanupUsesPostDistributionListing(t *testing.T) {
	staleRow := models.TemplateReplica{
		NodeID: "node-a", NodeIP: "10.0.0.1",
		Status: ReplicaStatusFailed, Phase: ReplicaPhaseFailed, CleanupRequired: true,
	}
	readyRow := models.TemplateReplica{
		NodeID: "node-a", NodeIP: "10.0.0.1",
		Status: ReplicaStatusReady, Phase: ReplicaPhaseReady,
	}
	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: "rfs-xfs-1"}

	// Converged case: FAILED in both pre-distribution listings, READY by the
	// pre-cleanup listing (a redo fixed the node while distribute ran).
	fresh := newBackfillPrepareFixture(t, "xfs")
	fresh.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	fresh.replicasSeq = [][]models.TemplateReplica{{staleRow}, {readyRow}}
	fresh.stub(t)
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(fresh.cleanups) != 1 {
		t.Fatalf("cleanups = %d, want 1", len(fresh.cleanups))
	}
	if stale := fresh.cleanups[0].replicas; len(stale) != 0 {
		t.Fatalf("cleanup stale set = %+v, want empty (node-a converged to READY before cleanup)", stale)
	}

	// Control: the row is still FAILED at the pre-cleanup listing, so the
	// fresh view keeps it in the stale set.
	still := newBackfillPrepareFixture(t, "xfs")
	still.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	still.replicasSeq = [][]models.TemplateReplica{{staleRow}, {staleRow}}
	still.stub(t)
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(still.cleanups) != 1 {
		t.Fatalf("cleanups = %d, want 1", len(still.cleanups))
	}
	if stale := still.cleanups[0].replicas; len(stale) != 1 || stale[0].NodeID != "node-a" {
		t.Fatalf("cleanup stale set = %+v, want the still-FAILED node-a row", stale)
	}
}

// A failed pre-cleanup re-list must not abort the sweep: the cleanup is
// skipped (warn-logged), the prepare still runs on the pre-distribution
// readyTargets, and the outcome is persisted and registered.
func TestBackfillPrepareRunsWhenCleanupListingFails(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.replicasSeq = [][]models.TemplateReplica{{}, {}, {
		{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady, Phase: ReplicaPhaseReady},
	}}
	f.replicasSeqErr = []error{nil, errors.New("list replicas fail"), nil}
	f.stub(t)
	t.Cleanup(func() {
		invalidateTemplateCaches("tpl-cleanup-list")
		localcache.InvalidateImageState("tpl-cleanup-list")
	})

	def := backfillDefinition{TemplateID: "tpl-cleanup-list", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("a failed cleanup listing must be logged, not returned: %v", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("distribute calls = %d, want 1", len(f.distCalls))
	}
	if len(f.cleanups) != 0 {
		t.Fatalf("cleanups = %d, want 0 (cleanup skipped when the fresh listing fails)", len(f.cleanups))
	}
	upserts := f.upsertsSnapshot()
	if len(f.snapshotCalls) != 1 || len(upserts) != 1 || upserts[0].replica.Status != ReplicaStatusReady {
		t.Fatalf("snap=%d upserts=%+v, want 1/1 READY (prepare still runs on the pre-distribution targets)",
			len(f.snapshotCalls), upserts)
	}
	if state := localcache.GetImageStateByNode("tpl-cleanup-list", "node-a"); state == nil {
		t.Fatalf("READY upsert must still register node-a despite the failed cleanup listing")
	}
}

// The re-register after a successful prepare re-reads the rows because
// InvalidateImageState wiped the whole template's registrations. When THAT
// listing fails the sweep must fall back to registering the freshly prepared
// subset — registering nothing would drop the node from scheduling entirely.
func TestBackfillRegisterFallsBackToPreparedOnFreshListingError(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.replicasSeq = [][]models.TemplateReplica{{}, {}, {}}
	f.replicasSeqErr = []error{nil, nil, errors.New("fresh list fail")}
	f.stub(t)
	t.Cleanup(func() {
		invalidateTemplateCaches("tpl-regfallback")
		localcache.InvalidateImageState("tpl-regfallback")
	})

	def := backfillDefinition{TemplateID: "tpl-regfallback", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	upserts := f.upsertsSnapshot()
	if len(upserts) != 1 || upserts[0].replica.Status != ReplicaStatusReady {
		t.Fatalf("upserts = %+v, want one READY row", upserts)
	}
	if state := localcache.GetImageStateByNode("tpl-regfallback", "node-a"); state == nil {
		t.Fatalf("fresh-listing failure must fall back to registering the prepared subset: node-a not cached")
	}
}

// A replica-listing failure aborts the sweep before any node is touched.
func TestBackfillReplicaListingErrorAbortsSweep(t *testing.T) {
	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: "rfs-xfs-1"}

	pre := newBackfillPrepareFixture(t, "xfs")
	pre.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	pre.replicasSeq = [][]models.TemplateReplica{{}}
	pre.replicasSeqErr = []error{errors.New("list replicas fail")}
	pre.stub(t)
	if err := backfillTemplateReplicas(context.Background(), def); err == nil {
		t.Fatalf("listing failure must abort the sweep")
	}
	if len(pre.distCalls) != 0 || len(pre.snapshotCalls) != 0 || len(pre.upsertsSnapshot()) != 0 {
		t.Fatalf("listing failure must not touch nodes: dist=%d snap=%d upserts=%d",
			len(pre.distCalls), len(pre.snapshotCalls), len(pre.upsertsSnapshot()))
	}

}

// jfs: a placement-push failure must surface as the sweep error (the
// reconciler logs per-template and retries next tick); AppSnapshot and
// replica rows stay out of the jfs path regardless.
func TestBackfillJFSPlacementDistributeErrorSurfaces(t *testing.T) {
	f := newBackfillPrepareFixture(t, "jfs")
	f.nodes = []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-new", "10.0.0.4"),
	}
	f.placements = []models.ArtifactNodePlacement{
		{ArtifactID: f.artifact.ArtifactID, NodeID: "node-a", NodeIP: "10.0.0.1", InstanceType: "cubebox"},
	}
	f.distErr = errors.New("node-new placement push failed")
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	err := backfillTemplateReplicas(context.Background(), def)
	if err == nil || !errors.Is(err, f.distErr) {
		t.Fatalf("backfillTemplateReplicas err = %v, want it to carry the jfs distribute error", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("distribute calls = %d, want 1", len(f.distCalls))
	}
	if len(f.snapshotCalls) != 0 || len(f.upsertsSnapshot()) != 0 {
		t.Fatalf("jfs sweep must never run AppSnapshot or write replica rows, snap=%d upserts=%d",
			len(f.snapshotCalls), len(f.upsertsSnapshot()))
	}
}

// The artifact reference falls back to the job's ArtifactID when the
// definition carries none (definitions written before the column was
// backfilled); when neither has one there is nothing safe to push.
func TestBackfillArtifactReferenceFallsBackToJob(t *testing.T) {
	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox"}

	// Fallback: the definition row predates the artifact column, the job
	// still knows which artifact it built.
	withJob := newBackfillPrepareFixture(t, "xfs")
	withJob.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	withJob.job.ArtifactID = withJob.artifact.ArtifactID
	withJob.stub(t)
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas with job artifact fallback: %v", err)
	}
	if len(withJob.distCalls) != 1 || len(withJob.snapshotCalls) != 1 || len(withJob.upsertsSnapshot()) != 1 {
		t.Fatalf("job artifact fallback must run the full sweep: dist=%d snap=%d upserts=%d",
			len(withJob.distCalls), len(withJob.snapshotCalls), len(withJob.upsertsSnapshot()))
	}

	// Neither definition nor job names an artifact: abort before touching
	// any node.
	none := newBackfillPrepareFixture(t, "xfs")
	none.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	none.stub(t)
	if err := backfillTemplateReplicas(context.Background(), def); err == nil {
		t.Fatalf("backfillTemplateReplicas err = nil, want the missing-artifact-reference error")
	}
	if len(none.distCalls) != 0 || len(none.snapshotCalls) != 0 || len(none.upsertsSnapshot()) != 0 {
		t.Fatalf("a missing artifact reference must not touch nodes: dist=%d snap=%d upserts=%d",
			len(none.distCalls), len(none.snapshotCalls), len(none.upsertsSnapshot()))
	}
}

// A node-resolution failure must surface as the sweep error and run nothing:
// without trustworthy targets there is no safe push set.
func TestBackfillResolveNodesErrorAbortsSweep(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.resolveErr = errors.New("node resolver unavailable")
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err == nil {
		t.Fatalf("resolveNodes failure must abort the sweep")
	}
	if len(f.distCalls) != 0 || len(f.snapshotCalls) != 0 || len(f.upsertsSnapshot()) != 0 {
		t.Fatalf("resolveNodes failure must not touch nodes: dist=%d snap=%d upserts=%d",
			len(f.distCalls), len(f.snapshotCalls), len(f.upsertsSnapshot()))
	}
}

// jfs: the resolver runs before the placement listing; its failure must
// abort the jfs push just the same.
func TestBackfillJFSResolveNodesErrorAbortsSweep(t *testing.T) {
	f := newBackfillPrepareFixture(t, "jfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.resolveErr = errors.New("node resolver unavailable")
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err == nil {
		t.Fatalf("jfs resolveNodes failure must abort the sweep")
	}
	if len(f.distCalls) != 0 {
		t.Fatalf("jfs resolveNodes failure must not distribute, calls = %d", len(f.distCalls))
	}
}

// The sweep must refuse to push an artifact row that is not distributable
// (ensureArtifactDistributable): bypassing the check would hand every
// cubelet a download URL that can only 404.
func TestBackfillUnservableArtifactAbortsSweep(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.artifact.Status = ArtifactStatusFailed
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err == nil {
		t.Fatalf("a non-READY artifact must abort the sweep")
	}
	if len(f.distCalls) != 0 {
		t.Fatalf("a non-READY artifact must never reach distribute, calls = %d", len(f.distCalls))
	}
}

// A garbage RequestJSON snapshot must abort the sweep before any push: the
// sweep cannot reconstruct the per-template request from it.
func TestBackfillCorruptJobSnapshotAbortsSweep(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.job.RequestJSON = "{not-json"
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err == nil {
		t.Fatalf("an undecodable job snapshot must abort the sweep")
	}
	if len(f.distCalls) != 0 {
		t.Fatalf("an undecodable job snapshot must abort before distribute, calls = %d", len(f.distCalls))
	}
}

// A sweep whose context is already canceled must not run AppSnapshot
// prepares or persist rows: once the reconcile pass has been asked to stop,
// writing half a sweep's worth of rows is worse than writing none.
func TestBackfillCanceledContextSkipsPrepareAndPersist(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{testNode("node-a", "10.0.0.1")}
	f.stub(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	def := backfillDefinition{TemplateID: "tpl-1", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(ctx, def); err != nil {
		t.Fatalf("a canceled sweep must not surface an error: %v", err)
	}
	if len(f.snapshotCalls) != 0 || len(f.upsertsSnapshot()) != 0 {
		t.Fatalf("canceled sweep must skip prepare and persist: snap=%d upserts=%+v",
			len(f.snapshotCalls), f.upsertsSnapshot())
	}
}

// Rows our own distribute just wrote carry (FAILED, DISTRIBUTED,
// CleanupRequired=false): they cannot correspond to an existing snapshot
// volume (only AppSnapshot creates one), so the stale cleanup must skip
// exactly that row shape. Both conjuncts are pinned here with adversarial
// siblings: a DISTRIBUTED row flagged cleanup_required (e.g. DeleteTemplate
// marked an in-flight row) is a genuine leftover and MUST be cleaned, and a
// legacy no-phase row (written before the phase column) with the flag clear
// is not the in-flight shape either and MUST be cleaned. Excluding by Phase
// alone would drop the former; excluding by CleanupRequired alone would drop
// the latter.
func TestBackfillCleanupCoversJustPushedInFlightRows(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
	}
	leftoverRow := models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed, Phase: ReplicaPhaseFailed, CleanupRequired: true}
	// The pre-distribute listing shows the leftover only; the post-distribute
	// listing adds node-a's freshly written in-flight DISTRIBUTED row.
	f.replicasSeq = [][]models.TemplateReplica{
		{leftoverRow},
		{{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusFailed, Phase: ReplicaPhaseDistributed}, leftoverRow},
	}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-inflight", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	t.Cleanup(func() { invalidateTemplateCaches("tpl-inflight"); localcache.InvalidateImageState("tpl-inflight") })
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.cleanups) != 1 {
		t.Fatalf("cleanups = %d, want 1", len(f.cleanups))
	}
	stale := f.cleanups[0].replicas
	got := make(map[string]bool, len(stale))
	for _, r := range stale {
		got[r.NodeID] = true
	}
	// The in-flight row must be cleaned too: the node's package may predate
	// the row (a lost row does not remove the package), and AppSnapshot
	// refuses to overwrite it — skipping the cleanup deadlocks the sweep.
	if len(stale) != 2 || !got["node-a"] || !got["node-b"] {
		t.Fatalf("stale set = %+v, want node-a (in-flight DISTRIBUTED) and node-b (leftover)", stale)
	}
}

// One sweep must not push + prepare an unbounded node set while holding the
// cluster-wide reconcile lock: the target list is capped and the remainder
// is left for the next tick.
func TestBackfillSweepCapsTargetsPerRound(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = make([]*node.Node, 0, backfillMaxTargetsPerSweep+6)
	for i := 0; i < backfillMaxTargetsPerSweep+6; i++ {
		f.nodes = append(f.nodes, testNode(fmt.Sprintf("node-capped-%03d", i), fmt.Sprintf("10.1.%d.%d", i/254, i%254+1)))
	}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-cap", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	t.Cleanup(func() { invalidateTemplateCaches("tpl-cap"); localcache.InvalidateImageState("tpl-cap") })
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("distribute calls = %d, want 1", len(f.distCalls))
	}
	if got := len(f.distCalls[0].targets); got != backfillMaxTargetsPerSweep {
		t.Fatalf("distribute targets = %d, want capped at %d (remainder next tick)", got, backfillMaxTargetsPerSweep)
	}
}

// The cap defers work, it does not drop it: nodes beyond the cap stay
// missing (no READY row was ever written for them) and the next sweep must
// distribute + prepare exactly those.
func TestBackfillSweepPicksUpRemainderNextRound(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	f.nodes = make([]*node.Node, 0, backfillMaxTargetsPerSweep+6)
	for i := 0; i < backfillMaxTargetsPerSweep+6; i++ {
		f.nodes = append(f.nodes, testNode(fmt.Sprintf("node-capped-%03d", i), fmt.Sprintf("10.1.%d.%d", i/254, i%254+1)))
	}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-remainder", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	t.Cleanup(func() { invalidateTemplateCaches("tpl-remainder"); localcache.InvalidateImageState("tpl-remainder") })
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if got := len(f.distCalls[0].targets); got != backfillMaxTargetsPerSweep {
		t.Fatalf("first sweep targets = %d, want capped at %d", got, backfillMaxTargetsPerSweep)
	}

	// First sweep converged the capped subset: its nodes now hold READY rows.
	f.mu.Lock()
	for i := 0; i < backfillMaxTargetsPerSweep; i++ {
		f.replicas = append(f.replicas, models.TemplateReplica{
			NodeID: fmt.Sprintf("node-capped-%03d", i), Status: ReplicaStatusReady, Phase: ReplicaPhaseReady,
		})
	}
	f.mu.Unlock()

	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(f.distCalls) != 2 {
		t.Fatalf("distribute calls = %d, want 2 (one per sweep)", len(f.distCalls))
	}
	remainder := f.distCalls[1].targets
	if len(remainder) != 6 {
		t.Fatalf("second sweep targets = %d, want the 6 remaining nodes", len(remainder))
	}
	for i, target := range remainder {
		wantID := fmt.Sprintf("node-capped-%03d", backfillMaxTargetsPerSweep+i)
		if target.ID() != wantID {
			t.Fatalf("second sweep target[%d] = %s, want %s (capped-out nodes, in order)", i, target.ID(), wantID)
		}
	}
	if got := len(f.snapshotCalls); got != backfillMaxTargetsPerSweep+6 {
		t.Fatalf("AppSnapshot calls = %d, want %d (remainder prepared in round two)", got, backfillMaxTargetsPerSweep+6)
	}
}

// Nodes with no replica row must go ahead of FAILED retries when the cap
// truncates: retries re-enter missing every sweep, so a persistent failure
// block would otherwise occupy the head forever and starve the tail.
func TestBackfillSweepPrioritizesFreshNodesOverRetries(t *testing.T) {
	f := newBackfillPrepareFixture(t, "xfs")
	total := backfillMaxTargetsPerSweep + 2
	f.nodes = make([]*node.Node, 0, total)
	failedRows := make([]models.TemplateReplica, 0, backfillMaxTargetsPerSweep)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("node-prio-%03d", i)
		f.nodes = append(f.nodes, testNode(id, fmt.Sprintf("10.2.%d.%d", i/254, i%254+1)))
		// The first 64 nodes carry a FAILED row (deterministic retries);
		// the last two are fresh. Adversarial normalization keys: node-000's
		// row carries only the IP, node-001's only the ID — an implementation
		// matching rows on just one key misclassifies that node as fresh and
		// lands it in the capped head.
		if i < backfillMaxTargetsPerSweep {
			row := models.TemplateReplica{NodeID: id, NodeIP: fmt.Sprintf("10.2.%d.%d", i/254, i%254+1), Status: ReplicaStatusFailed, Phase: ReplicaPhaseFailed, CleanupRequired: true}
			if i == 0 {
				row.NodeID = ""
			}
			if i == 1 {
				row.NodeIP = ""
			}
			failedRows = append(failedRows, row)
		}
	}
	f.replicas = failedRows
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-prio", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	t.Cleanup(func() { invalidateTemplateCaches("tpl-prio"); localcache.InvalidateImageState("tpl-prio") })
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("distribute calls = %d, want 1", len(f.distCalls))
	}
	targets := f.distCalls[0].targets
	if len(targets) != backfillMaxTargetsPerSweep {
		t.Fatalf("distribute targets = %d, want capped at %d", len(targets), backfillMaxTargetsPerSweep)
	}
	for i, want := range []string{"node-prio-064", "node-prio-065"} {
		if targets[i].ID() != want {
			t.Fatalf("targets[%d] = %s, want fresh node %s (fresh nodes must precede FAILED retries)", i, targets[i].ID(), want)
		}
	}
}

// The jfs placement push is capped by the same per-sweep budget: it runs
// under the same cluster-wide reconcile lock, so an unbounded node set
// would starve the other sweeps just the same.
func TestBackfillJFSSweepCapsTargetsPerRound(t *testing.T) {
	f := newBackfillPrepareFixture(t, "jfs")
	f.nodes = make([]*node.Node, 0, backfillMaxTargetsPerSweep+6)
	for i := 0; i < backfillMaxTargetsPerSweep+6; i++ {
		f.nodes = append(f.nodes, testNode(fmt.Sprintf("node-jfscap-%03d", i), fmt.Sprintf("10.3.%d.%d", i/254, i%254+1)))
	}
	f.stub(t)

	def := backfillDefinition{TemplateID: "tpl-jfscap", InstanceType: "cubebox", RootfsArtifactID: f.artifact.ArtifactID}
	if err := backfillTemplateReplicas(context.Background(), def); err != nil {
		t.Fatalf("backfillTemplateReplicas: %v", err)
	}
	if len(f.distCalls) != 1 {
		t.Fatalf("distribute calls = %d, want 1", len(f.distCalls))
	}
	call := f.distCalls[0]
	if call.write != replicaWriteNone {
		t.Fatalf("jfs placement push must not track replica rows")
	}
	if got := len(call.targets); got != backfillMaxTargetsPerSweep {
		t.Fatalf("jfs distribute targets = %d, want capped at %d", got, backfillMaxTargetsPerSweep)
	}
}

// The jfs cap rotates the window every tick (jfs failures leave no row
// behind, so there is no FAILED-row signal to deprioritize a persistent
// failure block); rotation must preserve coverage: any offset yields every
// node exactly once, with the window starting at offset modulo len.
func TestBackfillRotateWindow(t *testing.T) {
	mk := func(n int) []*node.Node {
		out := make([]*node.Node, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, testNode(fmt.Sprintf("node-rot-%03d", i), fmt.Sprintf("10.9.%d.%d", i/254, i%254+1)))
		}
		return out
	}
	total := backfillMaxTargetsPerSweep + 6
	for _, offset := range []int{0, 1, 64, total - 1, total, total + 5, 2*total + 3} {
		got := backfillRotateWindow(mk(total), offset)
		if len(got) != total {
			t.Fatalf("offset %d: len = %d, want %d", offset, len(got), total)
		}
		wantFirst := offset % total
		if got[0].ID() != fmt.Sprintf("node-rot-%03d", wantFirst) {
			t.Fatalf("offset %d: first = %s, want node-rot-%03d", offset, got[0].ID(), wantFirst)
		}
		seen := make(map[string]int, total)
		for _, n := range got {
			seen[n.ID()]++
		}
		for i := 0; i < total; i++ {
			if seen[fmt.Sprintf("node-rot-%03d", i)] != 1 {
				t.Fatalf("offset %d: node-rot-%03d count = %d, want exactly 1 (rotation must not lose or duplicate nodes)", offset, i, seen[fmt.Sprintf("node-rot-%03d", i)])
			}
		}
	}
}
