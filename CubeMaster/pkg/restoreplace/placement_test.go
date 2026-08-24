// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package restoreplace

import (
	"context"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func originFactsJSON() string {
	return `{"cpuid_hash":"sha256:cpu","host_kernel_release":"5.15.0"}`
}

func resetPlacementSeams(t *testing.T) {
	t.Helper()
	origLookup := lookupNodeFn
	origSelect := selectNodeFn
	origList := listCompatibleFn
	t.Cleanup(func() {
		lookupNodeFn = origLookup
		selectNodeFn = origSelect
		listCompatibleFn = origList
	})
}

func TestCanCrossNode(t *testing.T) {
	t.Parallel()
	if CanCrossNode(constants.SnapshotBackendXFS, constants.RemoteStatusReady) {
		t.Fatal("xfs must never cross")
	}
	if CanCrossNode(constants.SnapshotBackendS3, constants.RemoteStatusPending) {
		t.Fatal("s3 pending must not cross")
	}
	if CanCrossNode(constants.SnapshotBackendS3, "") {
		t.Fatal("s3 empty remote_status must not cross")
	}
	if !CanCrossNode(constants.SnapshotBackendS3, constants.RemoteStatusReady) {
		t.Fatal("s3 ready must allow cross")
	}
	if !CanCrossNode("S3", "READY") {
		t.Fatal("s3 ready must be case-insensitive")
	}
}

func TestDecidePrefersOriginWhenSchedulable(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		return origin, nil
	}
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		t.Fatal("must not list compatible nodes when origin can schedule")
		return nil, nil
	}

	got, err := Decide(context.Background(), Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusReady,
		OriginNodeID:        "node-a",
		OriginHostFactsJSON: originFactsJSON(),
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CrossNode || got.NodeID != "node-a" {
		t.Fatalf("want origin node-a, got %+v", got)
	}
}

func TestDecideHostMountDoesNotCross(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: false}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		t.Fatal("host-mount must not select a peer")
		return nil, nil
	}
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		t.Fatal("host-mount must not list compatible nodes")
		return nil, nil
	}

	_, err := Decide(context.Background(), Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusReady,
		OriginNodeID:        "node-a",
		OriginHostFactsJSON: originFactsJSON(),
		PinToOrigin:         true,
	})
	if err == nil || !strings.Contains(err.Error(), "host-mount") {
		t.Fatalf("want host-mount origin-only failure, got %v", err)
	}
}

func TestDecideXFSDoesNotCrossWhenOriginFails(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: false}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		t.Fatal("xfs must not select a peer")
		return nil, nil
	}

	_, err := Decide(context.Background(), Input{
		SnapshotID:   "snap-1",
		Backend:      constants.SnapshotBackendXFS,
		OriginNodeID: "node-a",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot restore cross-node") {
		t.Fatalf("want origin-only failure, got %v", err)
	}
}

func TestDecideS3PendingDoesNotCross(t *testing.T) {
	resetPlacementSeams(t)
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return nil }

	_, err := Decide(context.Background(), Input{
		SnapshotID:   "snap-1",
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusPending,
		OriginNodeID: "node-a",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot restore cross-node") {
		t.Fatalf("want origin-only failure for pending sync, got %v", err)
	}
}

func TestDecideCrossNodeWhenOriginCannotSchedule(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: false}
	peer := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		if origin.CPUIDHash != "sha256:cpu" || origin.HostKernelRelease != "5.15.0" {
			t.Fatalf("unexpected origin facts %+v", origin)
		}
		return []compatibleCandidate{
			{NodeID: "node-a", NodeIP: "10.0.0.1"},
			{NodeID: "node-b", NodeIP: "10.0.0.2"},
		}, nil
	}
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		if reqRes == nil || !reqRes.AllowNonLocalTemplate {
			t.Fatal("cross-node select must allow non-local template")
		}
		found := false
		for _, s := range scope {
			if s == "node-b" {
				found = true
			}
			if s == "node-a" {
				t.Fatal("origin must be excluded from cross-node scope")
			}
		}
		if !found {
			t.Fatalf("peer missing from scope %v", scope)
		}
		return peer, nil
	}

	got, err := Decide(context.Background(), Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusReady,
		OriginNodeID:        "node-a",
		OriginHostFactsJSON: originFactsJSON(),
		InstanceType:        "cubebox",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.CrossNode || got.NodeID != "node-b" {
		t.Fatalf("want cross-node node-b, got %+v", got)
	}
}

func TestDecideCrossNodeRequiresHostFacts(t *testing.T) {
	resetPlacementSeams(t)
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return nil }

	_, err := Decide(context.Background(), Input{
		SnapshotID:   "snap-1",
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
		OriginNodeID: "node-a",
	})
	if err == nil || !strings.Contains(err.Error(), "cpuid_hash") {
		t.Fatalf("want fingerprint error, got %v", err)
	}
}

func TestDecideCrossNodeRejectsPickOutsideScope(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: false}
	outsider := &node.Node{InsID: "node-z", IP: "10.0.0.9", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		return []compatibleCandidate{{NodeID: "node-b", NodeIP: "10.0.0.2"}}, nil
	}
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		return outsider, nil
	}

	_, err := Decide(context.Background(), Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusReady,
		OriginNodeID:        "node-a",
		OriginHostFactsJSON: originFactsJSON(),
	})
	if err == nil || !strings.Contains(err.Error(), "outside compatible scope") {
		t.Fatalf("want pick-outside-scope error, got %v", err)
	}
}

func TestDecideCrossNodeNoCompatiblePeer(t *testing.T) {
	resetPlacementSeams(t)
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return nil }
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		return nil, nil
	}

	_, err := Decide(context.Background(), Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusReady,
		OriginNodeID:        "node-a",
		OriginHostFactsJSON: originFactsJSON(),
	})
	if err == nil || !strings.Contains(err.Error(), "no compatible node") {
		t.Fatalf("want no-compatible-node error, got %v", err)
	}
}
