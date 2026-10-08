// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func stubAppSnapshot(t *testing.T, snapshot func(nodeIP string) (*cubeboxv1.AppSnapshotResponse, error)) (appSnapshotCalls *int32) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	calls := new(int32)
	patches.ApplyFunc(cubelet.AppSnapshot, func(_ context.Context, ep string, _ *cubeboxv1.AppSnapshotRequest) (*cubeboxv1.AppSnapshotResponse, error) {
		*calls++
		return snapshot(ep)
	})
	return calls
}

func stubNodeRPCs(t *testing.T, snapshot func(nodeIP string) (*cubeboxv1.AppSnapshotResponse, error)) (appSnapshotCalls *int32) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(sandbox.ConstructCubeletReq, func(context.Context, *sandboxtypes.CreateCubeSandboxReq) (*cubeboxv1.RunCubeSandboxRequest, error) {
		return &cubeboxv1.RunCubeSandboxRequest{}, nil
	})
	return stubAppSnapshot(t, snapshot)
}

func okSnapshot() *cubeboxv1.AppSnapshotResponse {
	return &cubeboxv1.AppSnapshotResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success}}
}

func replicaReq() *sandboxtypes.CreateCubeSandboxReq {
	return &sandboxtypes.CreateCubeSandboxReq{InstanceType: "cubebox"}
}

func attrOf(span sdktrace.ReadOnlySpan, key string) (attribute.KeyValue, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv, true
		}
	}
	return attribute.KeyValue{}, false
}

func TestReplicaNodeSpanRecordsSuccessAndBusinessFailureRetCode(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubNodeRPCs(t, func(nodeIP string) (*cubeboxv1.AppSnapshotResponse, error) {
		if nodeIP == "10.0.0.2" {
			return &cubeboxv1.AppSnapshotResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Conflict, RetMsg: "still in use"}}, nil
		}
		return okSnapshot(), nil
	})

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	opts := replicaRunOptions{ArtifactID: "rfs-1", JobID: "job-1"}
	ok, _ := createReplicaOnNode(rootCtx, &node.Node{InsID: "node-ok", IP: "10.0.0.1"}, "tpl-1", replicaReq(), opts)
	if ok.Status != ReplicaStatusReady {
		t.Fatalf("healthy replica status = %q, want ready", ok.Status)
	}
	bad, _ := createReplicaOnNode(rootCtx, &node.Node{InsID: "node-bad", IP: "10.0.0.2"}, "tpl-1", replicaReq(), opts)
	if bad.Status != ReplicaStatusFailed {
		t.Fatalf("business-failure replica status = %q, want failed", bad.Status)
	}
	flush()

	byNode := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.snapshot() {
		if s.Name() != telemetry.SpanTemplateNodeSnapshot {
			continue
		}
		kv, _ := attrOf(s, telemetry.AttrNodeID)
		byNode[kv.Value.AsString()] = s
	}
	if len(byNode) != 2 {
		t.Fatalf("want 2 node spans, got %d", len(byNode))
	}
	for _, id := range []string{"node-ok", "node-bad"} {
		s, present := byNode[id]
		if !present {
			t.Fatalf("no span for %s", id)
		}
		for key, want := range map[string]string{
			telemetry.AttrArtifactID: "rfs-1",
			telemetry.AttrJobID:      "job-1",
			telemetry.AttrTemplateID: "tpl-1",
		} {
			kv, ok := attrOf(s, key)
			if !ok || kv.Value.AsString() != want {
				t.Errorf("%s attr %s = %q, want %q", id, key, kv.Value.AsString(), want)
			}
		}
	}
	if got := byNode["node-ok"].Status().Code; got != codes.Unset {
		t.Errorf("healthy node span status = %v, want Unset", got)
	}
	if _, present := attrOf(byNode["node-ok"], telemetry.AttrRetCode); present {
		t.Error("healthy node span must not carry a ret code")
	}
	badSpan := byNode["node-bad"]
	if badSpan.Status().Code != codes.Error {
		t.Errorf("failed node span status = %v, want Error", badSpan.Status().Code)
	}
	if kv, ok := attrOf(badSpan, telemetry.AttrRetCode); !ok || kv.Value.AsInt64() != int64(errorcodev1.ErrorCode_Conflict) {
		t.Errorf("failed node span ret code = %v, want %d", kv.Value.AsInt64(), errorcodev1.ErrorCode_Conflict)
	}
}

func TestReplicaNodeSpanCoversPreRPCParamValidationFailure(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	calls := stubAppSnapshot(t, func(string) (*cubeboxv1.AppSnapshotResponse, error) { return okSnapshot(), nil })

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	req := &sandboxtypes.CreateCubeSandboxReq{InstanceType: "cubebox"}
	replica, _ := createReplicaOnNode(rootCtx, &node.Node{InsID: "node-badreq", IP: "10.0.0.3"}, "tpl-2", req, replicaRunOptions{ArtifactID: "rfs-2", JobID: "job-2"})
	if replica.Status != ReplicaStatusFailed {
		t.Fatalf("rejected replica status = %q, want failed", replica.Status)
	}
	flush()

	if *calls != 0 {
		t.Errorf("AppSnapshot ran %d time(s) for a request rejected during validation", *calls)
	}
	var nodeSpan sdktrace.ReadOnlySpan
	for _, s := range rec.snapshot() {
		if s.Name() == telemetry.SpanTemplateNodeSnapshot {
			nodeSpan = s
		}
	}
	if nodeSpan == nil {
		t.Fatal("a node span must exist even when the request is rejected before the RPC")
	}
	if nodeSpan.Status().Code != codes.Error {
		t.Errorf("node span status = %v, want Error", nodeSpan.Status().Code)
	}
	if kv, ok := attrOf(nodeSpan, telemetry.AttrJobID); !ok || kv.Value.AsString() != "job-2" {
		t.Errorf("node span job id = %q, want job-2", kv.Value.AsString())
	}
}

func TestReplicaFanOutSpanFlagsPartialBusinessFailure(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(sandbox.ConstructCubeletReq, func(context.Context, *sandboxtypes.CreateCubeSandboxReq) (*cubeboxv1.RunCubeSandboxRequest, error) {
		return &cubeboxv1.RunCubeSandboxRequest{}, nil
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(cubelet.AppSnapshot, func(_ context.Context, ep string, _ *cubeboxv1.AppSnapshotRequest) (*cubeboxv1.AppSnapshotResponse, error) {
		if ep == "10.0.0.2" {
			return &cubeboxv1.AppSnapshotResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Conflict, RetMsg: "busy"}}, nil
		}
		return okSnapshot(), nil
	})
	patches.ApplyFunc(UpsertReplica, func(context.Context, string, string, ReplicaStatus) error { return nil })

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	replicas, err := createTemplateReplicasOnNodes(rootCtx, "tpl-3", replicaReq(),
		[]*node.Node{{InsID: "node-ok", IP: "10.0.0.1"}, {InsID: "node-bad", IP: "10.0.0.2"}},
		replicaRunOptions{ArtifactID: "rfs-3", JobID: "job-3"})
	if err != nil {
		t.Fatalf("a partial business failure must not surface as a persist error, got %v", err)
	}
	if len(replicas) != 2 {
		t.Fatalf("want 2 replica results, got %d", len(replicas))
	}
	flush()

	var agg sdktrace.ReadOnlySpan
	for _, s := range rec.snapshot() {
		if s.Name() == telemetry.SpanTemplateReplicate {
			agg = s
		}
	}
	if agg == nil {
		t.Fatal("no aggregate replicate span recorded")
	}
	if agg.Status().Code != codes.Error {
		t.Errorf("aggregate span status = %v, want Error when a sibling failed", agg.Status().Code)
	}
	if agg.SpanContext().TraceID() != rootSpan.SpanContext().TraceID() {
		t.Error("aggregate span did not continue the caller trace")
	}
}
