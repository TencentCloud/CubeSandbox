// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func stubEagerAdoptSeams(t *testing.T, nodes []*node.Node, resolveErr error, adopt func(ctx context.Context, target *node.Node) (*cubeboxv1.AdoptJFSTemplateResponse, error)) *[]string {
	t.Helper()
	var mu sync.Mutex
	called := &[]string{}
	origResolve, origAdopt := eagerAdoptJFSResolveNodes, eagerAdoptJFSNode
	t.Cleanup(func() {
		eagerAdoptJFSResolveNodes, eagerAdoptJFSNode = origResolve, origAdopt
	})
	eagerAdoptJFSResolveNodes = func(string, []string) ([]*node.Node, error) { return nodes, resolveErr }
	eagerAdoptJFSNode = func(_ context.Context, target *node.Node, _ string) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
		mu.Lock()
		*called = append(*called, target.HostIP())
		mu.Unlock()
		return adopt(context.Background(), target)
	}
	return called
}

func successAdopt(_ context.Context, _ *node.Node) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
	return &cubeboxv1.AdoptJFSTemplateResponse{
		Ret:     &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
		Adopted: true,
	}, nil
}

// A jfs template is eagerly adopted on every in-scope node before READY.
func TestEagerAdoptJFSFanOutToAllNodes(t *testing.T) {
	nodes := []*node.Node{testNode("a", "10.0.0.1"), testNode("b", "10.0.0.2"), testNode("c", "10.0.0.3")}
	called := stubEagerAdoptSeams(t, nodes, nil, successAdopt)
	req := &types.CreateTemplateFromImageReq{Backend: "jfs", InstanceType: "cubebox"}
	eagerAdoptJFSTemplate(context.Background(), req, nil, "tpl-1")
	if len(*called) != len(nodes) {
		t.Fatalf("adopt calls = %d, want %d", len(*called), len(nodes))
	}
}

// Non-jfs backends never trigger the eager fan-out, and neither does an
// empty template id or a node-resolution failure. Violations are recorded
// by the stub and asserted here, never from inside the fan-out goroutines.
func TestEagerAdoptJFSSkipsNonJFSBackends(t *testing.T) {
	for _, backend := range []string{"xfs", "s3", ""} {
		called := stubEagerAdoptSeams(t, []*node.Node{testNode("a", "10.0.0.1")}, nil, func(_ context.Context, _ *node.Node) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
			return nil, nil
		})
		req := &types.CreateTemplateFromImageReq{Backend: backend, InstanceType: "cubebox"}
		eagerAdoptJFSTemplate(context.Background(), req, nil, "tpl-1")
		if len(*called) != 0 {
			t.Fatalf("backend %q: adopt calls = %d, want 0", backend, len(*called))
		}
	}
	called := stubEagerAdoptSeams(t, []*node.Node{testNode("a", "10.0.0.1")}, nil, func(_ context.Context, _ *node.Node) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
		return nil, nil
	})
	eagerAdoptJFSTemplate(context.Background(), &types.CreateTemplateFromImageReq{Backend: "jfs"}, nil, "  ")
	if len(*called) != 0 {
		t.Fatalf("empty template id: adopt calls = %d, want 0", len(*called))
	}
	called = stubEagerAdoptSeams(t, nil, context.DeadlineExceeded, func(_ context.Context, _ *node.Node) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
		return nil, nil
	})
	eagerAdoptJFSTemplate(context.Background(), &types.CreateTemplateFromImageReq{Backend: "jfs"}, nil, "tpl-1")
	if len(*called) != 0 {
		t.Fatalf("resolution failure: adopt calls = %d, want 0", len(*called))
	}
}

// Node failures are best-effort: they must not abort the remaining fan-out,
// because first-use adoption is the fallback.
func TestEagerAdoptJFSNodeFailuresAreBestEffort(t *testing.T) {
	nodes := []*node.Node{testNode("a", "10.0.0.1"), testNode("b", "10.0.0.2")}
	called := stubEagerAdoptSeams(t, nodes, nil, func(_ context.Context, target *node.Node) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
		if target.HostIP() == "10.0.0.1" {
			return nil, context.DeadlineExceeded
		}
		return &cubeboxv1.AdoptJFSTemplateResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success}}, nil
	})
	req := &types.CreateTemplateFromImageReq{Backend: "jfs", InstanceType: "cubebox"}
	eagerAdoptJFSTemplate(context.Background(), req, nil, "tpl-1")
	if len(*called) != len(nodes) {
		t.Fatalf("adopt calls = %d, want %d (one failure must not stop the fan-out)", len(*called), len(nodes))
	}
}

// An empty jfs backend on the request falls back to the generated request's
// annotation, mirroring the rest of the template pipeline.
func TestEagerAdoptJFSBackendFromGeneratedRequest(t *testing.T) {
	nodes := []*node.Node{testNode("a", "10.0.0.1")}
	called := stubEagerAdoptSeams(t, nodes, nil, successAdopt)
	req := &types.CreateTemplateFromImageReq{InstanceType: "cubebox"}
	generatedReq := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{constants.CubeAnnotationStorageBackend: "jfs"},
	}
	eagerAdoptJFSTemplate(context.Background(), req, generatedReq, "tpl-1")
	if len(*called) != 1 {
		t.Fatalf("adopt calls = %d, want 1 (backend resolved from the generated request)", len(*called))
	}
}

func TestCreateJobStaysActiveUntilEagerAdoptionCompletes(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	target := testNode("node-a", "10.0.0.1")
	var terminalReady, adopted atomic.Bool
	stubEagerAdoptSeams(t, []*node.Node{target}, nil, func(_ context.Context, _ *node.Node) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
		// Runs inside a fan-out goroutine: record, never t.Fatal here.
		if terminalReady.Load() {
			return nil, nil
		}
		adopted.Store(true)
		return successAdopt(context.Background(), target)
	})

	patches.ApplyFunc(distributeRootfsArtifact, func(context.Context, *types.CreateTemplateFromImageReq, *types.CreateCubeSandboxReq, *models.RootfsArtifact, string, string) ([]*node.Node, int32, int32, int32, error) {
		return []*node.Node{target}, 1, 1, 0, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(_ context.Context, _ string, values map[string]any) error {
		if values["status"] == JobStatusReady {
			terminalReady.Store(true)
		}
		return nil
	})
	patches.ApplyFunc(normalizeStoredTemplateRequest, func(req *types.CreateCubeSandboxReq) (*types.CreateCubeSandboxReq, error) {
		return req, nil
	})
	patches.ApplyFunc(ensureTemplateDefinitionWithOptions, func(context.Context, string, *types.CreateCubeSandboxReq, string, string, definitionCreateOptions) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(createTemplateReplicasOnNodes, func(context.Context, string, *types.CreateCubeSandboxReq, []*node.Node, replicaRunOptions) ([]ReplicaStatus, error) {
		return []ReplicaStatus{{NodeID: target.ID(), NodeIP: target.HostIP(), Status: ReplicaStatusReady}}, nil
	})
	patches.ApplyFunc(finalizeTemplateReplicas, func(context.Context, string, string, string, string, []ReplicaStatus) (*TemplateInfo, string, error) {
		return &TemplateInfo{TemplateID: "tpl-1", Status: StatusReady}, "", nil
	})
	patches.ApplyFunc(preserveRemoteBuildReport, func(context.Context, string, []byte) []byte {
		return []byte(`{}`)
	})

	req := &types.CreateTemplateFromImageReq{
		TemplateID:   "tpl-1",
		InstanceType: "cubebox",
		Backend:      "jfs",
	}
	generatedReq := &types.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Annotations:  map[string]string{constants.CubeAnnotationStorageBackend: "jfs"},
	}
	err := finishTemplateImageJobAfterArtifact(context.Background(), "job-1", req, &models.RootfsArtifact{ArtifactID: "artifact-1"}, generatedReq, false)
	if err != nil {
		t.Fatalf("finishTemplateImageJobAfterArtifact() error = %v", err)
	}
	if !adopted.Load() || !terminalReady.Load() {
		t.Fatalf("adopted=%v terminalReady=%v, want both true", adopted.Load(), terminalReady.Load())
	}
	if terminalReady.Load() && !adopted.Load() {
		t.Fatal("job became READY before eager adoption completed")
	}
}
