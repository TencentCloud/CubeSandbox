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
	origPinned := selectPinnedFn
	origList := listCompatibleFn
	origColdBoot := listColdBootFn
	origQuery := queryHostFactCandidatesFn
	origMachineType := listMachineTypeCandidatesFn
	t.Cleanup(func() {
		lookupNodeFn = origLookup
		selectNodeFn = origSelect
		selectPinnedFn = origPinned
		listCompatibleFn = origList
		listColdBootFn = origColdBoot
		queryHostFactCandidatesFn = origQuery
		listMachineTypeCandidatesFn = origMachineType
	})
}

func isolatedOrigin(id, ip string) *node.Node {
	n := &node.Node{InsID: id, IP: ip, Healthy: true}
	n.SetSchedulingDisabled(true)
	return n
}

func hostFacts(cpuid, kernel string) *nodemeta.HostFacts {
	return &nodemeta.HostFacts{CPUIDHash: cpuid, HostKernelRelease: kernel}
}

func s3ReadyInput(originID string) Input {
	return Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusReady,
		OriginNodeID:        originID,
		OriginHostFactsJSON: originFactsJSON(),
		InstanceType:        "cubebox",
	}
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
	if CanCrossNode(constants.SnapshotBackendJFS, constants.RemoteStatusInProgress) {
		t.Fatal("jfs with blocks still staged must not cross")
	}
	if !CanCrossNode(constants.SnapshotBackendJFS, constants.RemoteStatusReady) {
		t.Fatal("jfs ready must allow cross")
	}
}

func TestDecidePrefersOriginWhenSchedulable(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectPinnedFn = func(sel *selctx.SelectorCtx, pinned node.NodeList) (*node.Node, error) {
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

func TestDecideS3InprogressDoesNotCross(t *testing.T) {
	resetPlacementSeams(t)
	origin := isolatedOrigin("node-a", "10.0.0.1")
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		t.Fatal("inprogress remote must not query peers")
		return nil, nil
	}

	_, err := Decide(context.Background(), Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusInProgress,
		OriginNodeID:        "node-a",
		OriginHostFactsJSON: originFactsJSON(),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot restore cross-node") {
		t.Fatalf("want origin-only failure for inprogress sync, got %v", err)
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

// Isolate leaves HEALTHY=true and only flips scheduling-disabled. Origin
// must still be treated as "cannot stay" and fall through to cross-node.
// Origin is HEALTHY and scheduling-allowed, but origin-scoped Select
// returns nothing (no capacity / CPU / mem). Stay-local fails, then
// cross-node Select on the kernel+cpuid peer list must run.
func TestDecideCrossNodeWhenOriginSelectFails(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	peer := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		for _, s := range scope {
			if s == "node-a" || s == "10.0.0.1" {
				return nil, nil
			}
		}
		return peer, nil
	}
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		return []compatibleCandidate{{NodeID: "node-b", NodeIP: "10.0.0.2"}}, nil
	}

	got, err := Decide(context.Background(), s3ReadyInput("node-a"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.CrossNode || got.NodeID != "node-b" {
		t.Fatalf("want cross-node node-b after origin Select miss, got %+v", got)
	}
}

func TestDecideCrossNodeWhenOriginIsolated(t *testing.T) {
	resetPlacementSeams(t)
	origin := isolatedOrigin("node-a", "10.0.0.1")
	peer := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		if len(scope) == 1 && (scope[0] == "node-a" || scope[0] == "10.0.0.1") {
			return nil, nil
		}
		for _, s := range scope {
			if s == "node-a" {
				t.Fatal("isolated origin must be excluded from cross-node scope")
			}
		}
		return peer, nil
	}
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		return []compatibleCandidate{{NodeID: "node-b", NodeIP: "10.0.0.2"}}, nil
	}

	got, err := Decide(context.Background(), s3ReadyInput("node-a"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.CrossNode || got.NodeID != "node-b" {
		t.Fatalf("want cross-node node-b after isolate, got %+v", got)
	}
}

func TestDecideIsolatedOriginDoesNotCrossUnlessS3Ready(t *testing.T) {
	resetPlacementSeams(t)
	origin := isolatedOrigin("node-a", "10.0.0.1")
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		return nil, nil
	}
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		t.Fatal("pending remote must not query peers")
		return nil, nil
	}

	_, err := Decide(context.Background(), Input{
		SnapshotID:          "snap-1",
		Backend:             constants.SnapshotBackendS3,
		RemoteStatus:        constants.RemoteStatusPending,
		OriginNodeID:        "node-a",
		OriginHostFactsJSON: originFactsJSON(),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot restore cross-node") {
		t.Fatalf("want origin-only failure, got %v", err)
	}
}

// queryHostFactCandidatesFn is the DB seam: SQL already equality-filters
// cpuid_hash + host_kernel_release. Decide's cross-node scope is exactly
// those rows, minus origin.
func TestDecideCrossNodeScopeFollowsHostFactQuery(t *testing.T) {
	resetPlacementSeams(t)
	origin := isolatedOrigin("node-a", "10.0.0.1")
	peer := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	listCompatibleFn = defaultListCompatible
	queryHostFactCandidatesFn = func(ctx context.Context, cpuid, kernel string, matchAll bool) ([]*nodemeta.CandidateNode, error) {
		if matchAll {
			t.Fatal("restore placement must not drop the required-key SQL predicate")
		}
		if cpuid != "sha256:cpu" || kernel != "5.15.0" {
			t.Fatalf("query must use origin cpuid/kernel, got %s %s", cpuid, kernel)
		}
		same := hostFacts(cpuid, kernel)
		return []*nodemeta.CandidateNode{
			{NodeID: "node-a", HostIP: "10.0.0.1", HostFacts: same},
			{NodeID: "node-b", HostIP: "10.0.0.2", HostFacts: same},
		}, nil
	}
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		want := map[string]bool{"node-b": false, "10.0.0.2": false}
		for _, s := range scope {
			if s == "node-a" || s == "10.0.0.1" {
				t.Fatalf("origin leaked into cross-node scope: %v", scope)
			}
			if _, ok := want[s]; ok {
				want[s] = true
			} else {
				t.Fatalf("unexpected scope entry %q in %v", s, scope)
			}
		}
		for k, seen := range want {
			if !seen {
				t.Fatalf("missing %s in scope %v", k, scope)
			}
		}
		return peer, nil
	}

	got, err := Decide(context.Background(), s3ReadyInput("node-a"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.CrossNode || got.NodeID != "node-b" {
		t.Fatalf("want node-b, got %+v", got)
	}
}

func TestDecideCrossNodeFailsWhenQueryHasNoMatchingPeer(t *testing.T) {
	resetPlacementSeams(t)
	origin := isolatedOrigin("node-a", "10.0.0.1")
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		t.Fatal("must not Select when query returned no peer")
		return nil, nil
	}
	listCompatibleFn = defaultListCompatible
	queryHostFactCandidatesFn = func(ctx context.Context, cpuid, kernel string, matchAll bool) ([]*nodemeta.CandidateNode, error) {
		// DB equality filter: cpuid/kernel mismatch never comes back.
		return []*nodemeta.CandidateNode{
			{NodeID: "node-a", HostIP: "10.0.0.1", HostFacts: hostFacts(cpuid, kernel)},
		}, nil
	}

	_, err := Decide(context.Background(), s3ReadyInput("node-a"))
	if err == nil || !strings.Contains(err.Error(), "no compatible node") {
		t.Fatalf("want no-compatible-node, got %v", err)
	}
}

func TestDecideSkipsOriginOutsideRequestedScope(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	selectPinnedFn = func(sel *selctx.SelectorCtx, pinned node.NodeList) (*node.Node, error) {
		t.Fatal("origin outside the requested scope must not be checked")
		return origin, nil
	}
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		t.Fatal("xfs must not select a peer")
		return nil, nil
	}

	in := s3ReadyInput("node-a")
	in.Backend = constants.SnapshotBackendXFS
	in.RequestedScope = []string{"node-z"}
	_, err := Decide(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "cannot restore cross-node") {
		t.Fatalf("want origin-only failure, got %v", err)
	}
}

func TestDecideFilesystemColdBootIgnoresHostFacts(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: false}
	peer := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	listCompatibleFn = func(ctx context.Context, origin *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		t.Fatal("cold boot must not filter candidates by host facts")
		return nil, nil
	}
	listColdBootFn = func(ctx context.Context, instanceType string) ([]compatibleCandidate, error) {
		return []compatibleCandidate{{NodeID: peer.ID(), NodeIP: peer.HostIP()}}, nil
	}
	selectNodeFn = func(ctx context.Context, instanceType string, reqRes *selctx.RequestResource, scope []string) (*node.Node, error) {
		return peer, nil
	}

	got, err := Decide(context.Background(), Input{
		SnapshotID:   "snap-fs",
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
		OriginNodeID: "node-a",
		Kind:         FilesystemColdBoot,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.CrossNode || got.NodeID != "node-b" {
		t.Fatalf("want cross-node node-b, got %+v", got)
	}
}

func TestDecideMemoryRestoreStillRequiresHostFacts(t *testing.T) {
	resetPlacementSeams(t)
	origin := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: false}
	lookupNodeFn = func(nodeID, nodeIP string) *node.Node { return origin }
	listColdBootFn = func(ctx context.Context, instanceType string) ([]compatibleCandidate, error) {
		t.Fatal("memory restore must not use the cold-boot candidate list")
		return nil, nil
	}

	_, err := Decide(context.Background(), Input{
		SnapshotID:   "snap-full",
		Backend:      constants.SnapshotBackendS3,
		RemoteStatus: constants.RemoteStatusReady,
		OriginNodeID: "node-a",
		Kind:         MemoryRestore,
	})
	if err == nil || !strings.Contains(err.Error(), "cpuid_hash/host_kernel_release unknown") {
		t.Fatalf("want host-fact failure, got %v", err)
	}
}

// --- machine type candidate path ---

func TestDecideMachineTypeSameCpuidDifferentKernelAllowed(t *testing.T) {
	resetPlacementSeams(t)
	lookupNodeFn = func(id, ip string) *node.Node { return nil }
	selectNodeFn = func(_ context.Context, _ string, _ *selctx.RequestResource, scope []string) (*node.Node, error) {
		return &node.Node{InsID: "node-b", IP: "10.0.0.2"}, nil
	}
	listMachineTypeCandidatesFn = func(_ context.Context, cpuid, machineType string) ([]compatibleCandidate, error) {
		if cpuid != "sha256:cpu" {
			t.Fatalf("cpuid = %s", cpuid)
		}
		if machineType != "intel-k66" {
			t.Fatalf("machineType = %s", machineType)
		}
		return []compatibleCandidate{{NodeID: "node-b", NodeIP: "10.0.0.2"}}, nil
	}
	listCompatibleFn = func(context.Context, *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		t.Fatal("host facts path must not run when machine type is set")
		return nil, nil
	}

	in := s3ReadyInput("node-a")
	in.SourceMachineType = "intel-k66"
	got, err := Decide(context.Background(), in)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !got.CrossNode || got.NodeID != "node-b" {
		t.Fatalf("placement = %+v", got)
	}
}

func TestDecideMachineTypeNoCandidateDoesNotFallBack(t *testing.T) {
	resetPlacementSeams(t)
	lookupNodeFn = func(id, ip string) *node.Node { return nil }
	listMachineTypeCandidatesFn = func(context.Context, string, string) ([]compatibleCandidate, error) {
		return nil, nil
	}
	listCompatibleFn = func(context.Context, *nodemeta.HostFacts) ([]compatibleCandidate, error) {
		t.Fatal("must not fall back to host facts candidates")
		return nil, nil
	}

	in := s3ReadyInput("node-a")
	in.SourceMachineType = "intel-k66"
	_, err := Decide(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "machine type") {
		t.Fatalf("want machine-type candidate error, got %v", err)
	}
}

func TestDecideMachineTypeRequiresOriginCpuid(t *testing.T) {
	resetPlacementSeams(t)
	lookupNodeFn = func(id, ip string) *node.Node { return nil }

	in := s3ReadyInput("node-a")
	in.OriginHostFactsJSON = `{"host_kernel_release":"5.15.0"}`
	in.SourceMachineType = "intel-k66"
	_, err := Decide(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "cpuid_hash") {
		t.Fatalf("want cpuid error, got %v", err)
	}
}

func TestMachineTypeLabelExtraction(t *testing.T) {
	if got := MachineTypeLabel(map[string]string{MachineTypeLabelKey: " intel-k66 "}); got != "intel-k66" {
		t.Fatalf("got %q", got)
	}
	if got := MachineTypeLabel(nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultListMachineTypeCandidates_FiltersByLabelAndCPUID(t *testing.T) {
	prev := queryHostFactCandidatesFn
	t.Cleanup(func() { queryHostFactCandidatesFn = prev })
	queryHostFactCandidatesFn = func(_ context.Context, cpuid, kernel string, matchAll bool) ([]*nodemeta.CandidateNode, error) {
		if !matchAll || kernel != "" || cpuid != "" {
			t.Fatalf("expected a match-all query, got cpuid=%q kernel=%q matchAll=%v", cpuid, kernel, matchAll)
		}
		same := &nodemeta.HostFacts{CPUIDHash: "sha256:cpu"}
		return []*nodemeta.CandidateNode{
			{NodeID: "ok", HostIP: "10.0.0.1", HostFacts: same, NodeLabels: map[string]string{MachineTypeLabelKey: "intel-k66"}},
			{NodeID: "other-label", HostIP: "10.0.0.2", HostFacts: same, NodeLabels: map[string]string{MachineTypeLabelKey: "amd-k66"}},
			{NodeID: "no-label", HostIP: "10.0.0.3", HostFacts: same},
			{NodeID: "other-cpu", HostIP: "10.0.0.4", HostFacts: &nodemeta.HostFacts{CPUIDHash: "sha256:other"}, NodeLabels: map[string]string{MachineTypeLabelKey: "intel-k66"}},
			// Different kernel release is what the label is allowed to ignore.
			{NodeID: "other-kernel", HostIP: "10.0.0.5", HostFacts: &nodemeta.HostFacts{CPUIDHash: "sha256:cpu", HostKernelRelease: "6.6.other"}, NodeLabels: map[string]string{MachineTypeLabelKey: "intel-k66"}},
		}, nil
	}
	got, err := defaultListMachineTypeCandidates(context.Background(), "sha256:cpu", "intel-k66")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.NodeID)
	}
	if len(ids) != 2 || ids[0] != "ok" || ids[1] != "other-kernel" {
		t.Fatalf("candidates = %v, want [ok other-kernel]", ids)
	}
}

// The label path must not add a target-side kvm-taint filter: the only
// dimension it replaces is host_kernel_release.
func TestMachineTypeCandidatesDoNotAddExtraGates(t *testing.T) {
	prev := queryHostFactCandidatesFn
	t.Cleanup(func() { queryHostFactCandidatesFn = prev })
	queryHostFactCandidatesFn = func(context.Context, string, string, bool) ([]*nodemeta.CandidateNode, error) {
		return []*nodemeta.CandidateNode{{
			NodeID:     "tainted",
			HostIP:     "10.0.0.9",
			HostFacts:  &nodemeta.HostFacts{CPUIDHash: "sha256:cpu", KVMModuleTaint: "E"},
			NodeLabels: map[string]string{MachineTypeLabelKey: "intel-k66"},
		}}, nil
	}
	got, err := defaultListMachineTypeCandidates(context.Background(), "sha256:cpu", "intel-k66")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "tainted" {
		t.Fatalf("label path must not filter on kvm taint, got %+v", got)
	}
}
