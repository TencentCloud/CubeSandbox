// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func readyDistributeArtifact(id string) *models.RootfsArtifact {
	return &models.RootfsArtifact{
		ArtifactID:    id,
		Status:        ArtifactStatusReady,
		Ext4SizeBytes: 4096,
		Ext4SHA256:    "sha",
		DownloadToken: "tok",
		MasterNodeIP:  "http://master-from-row:8089",
	}
}

func requireSingleDistributeSpan(t *testing.T, rec *spanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.snapshot() {
		if s.Name() == telemetry.SpanTemplateDistribute {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %s span, got %d", telemetry.SpanTemplateDistribute, len(found))
	}
	return found[0]
}

func assertSpanStrAttr(t *testing.T, span sdktrace.ReadOnlySpan, key, want string) {
	t.Helper()
	kv, ok := attrOf(span, key)
	if !ok || kv.Value.AsString() != want {
		t.Errorf("span attr %s = %q (present=%t), want %q", key, kv.Value.AsString(), ok, want)
	}
}

func nodeImageChildCount(t *testing.T, rec *spanRecorder, parent sdktrace.ReadOnlySpan) int {
	t.Helper()
	count := 0
	for _, s := range rec.snapshot() {
		if s.Name() != telemetry.SpanTemplateNodeImage {
			continue
		}
		count++
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("node image span parent = %s, want distribute span %s",
				s.Parent().SpanID(), parent.SpanContext().SpanID())
		}
	}
	return count
}

func stubDistributeFanOut(t *testing.T, createImage func(ip string) (*imagev1.CreateImageRequestResponse, error)) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(verifyArtifactServability, func(context.Context, *models.RootfsArtifact) error { return nil })
	patches.ApplyFunc(resolveTemplateNodes, func(string, []string) ([]*node.Node, error) {
		return []*node.Node{{InsID: "n1", IP: "10.0.0.1"}, {InsID: "n2", IP: "10.0.0.2"}}, nil
	})
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(cubelet.CreateImage, func(_ context.Context, ip string, _ *imagev1.CreateImageRequest) (*imagev1.CreateImageRequestResponse, error) {
		return createImage(ip)
	})
	patches.ApplyFunc(UpsertReplica, func(context.Context, string, string, ReplicaStatus) error { return nil })
	patches.ApplyFunc(upsertArtifactNodePlacement, func(context.Context, string, string, string) error { return nil })
}

func okCreateImage() (*imagev1.CreateImageRequestResponse, error) {
	return &imagev1.CreateImageRequestResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success}}, nil
}

func TestDistributeSpanCoversPreFanOutFailures(t *testing.T) {
	tests := []struct {
		name             string
		artifact         *models.RootfsArtifact
		servabilityErr   error
		resolveErr       error
		wantArtifactAttr bool
		wantErrSubstr    string
	}{
		{
			name:          "nil-artifact",
			artifact:      nil,
			wantErrSubstr: "artifact is nil",
		},
		{
			name:             "incomplete-artifact",
			artifact:         &models.RootfsArtifact{ArtifactID: "rfs-incomplete", Status: ArtifactStatusBuilding},
			wantArtifactAttr: true,
			wantErrSubstr:    "not ready for distribution",
		},
		{
			name:             "unservable-artifact",
			artifact:         readyDistributeArtifact("rfs-unservable"),
			servabilityErr:   errors.New("download endpoint reports it missing"),
			wantArtifactAttr: true,
			wantErrSubstr:    "download endpoint reports it missing",
		},
		{
			name:             "node-resolution-failure",
			artifact:         readyDistributeArtifact("rfs-nonodes"),
			resolveErr:       ErrNoTemplateNodes,
			wantArtifactAttr: true,
			wantErrSubstr:    "no healthy nodes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := installSpanRecorder(t)
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			patches.ApplyFunc(verifyArtifactServability, func(context.Context, *models.RootfsArtifact) error {
				return tc.servabilityErr
			})
			if tc.resolveErr != nil {
				patches.ApplyFunc(resolveTemplateNodes, func(string, []string) ([]*node.Node, error) {
					return nil, tc.resolveErr
				})
			}

			rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
			defer rootSpan.End()

			_, _, _, _, err := distributeRootfsArtifact(rootCtx,
				&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
				&types.CreateCubeSandboxReq{InstanceType: "cubebox"},
				tc.artifact, "tpl-early", "job-early")
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("distributeRootfsArtifact() error = %v, want substring %q", err, tc.wantErrSubstr)
			}
			flush()

			span := requireSingleDistributeSpan(t, rec)
			if span.Status().Code != codes.Error {
				t.Errorf("distribute span status = %v, want Error", span.Status().Code)
			}
			if span.SpanContext().TraceID() != rootSpan.SpanContext().TraceID() {
				t.Error("distribute span did not continue the caller trace")
			}
			assertSpanStrAttr(t, span, telemetry.AttrJobID, "job-early")
			assertSpanStrAttr(t, span, telemetry.AttrTemplateID, "tpl-early")
			kv, haveArtifact := attrOf(span, telemetry.AttrArtifactID)
			if haveArtifact != tc.wantArtifactAttr {
				t.Fatalf("artifact attr present = %t, want %t", haveArtifact, tc.wantArtifactAttr)
			}
			if tc.wantArtifactAttr && kv.Value.AsString() != tc.artifact.ArtifactID {
				t.Errorf("artifact attr = %q, want %q", kv.Value.AsString(), tc.artifact.ArtifactID)
			}
		})
	}
}

func TestDistributeSpanCoversNodeFanOutAndParentsNodeSpans(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubDistributeFanOut(t, func(string) (*imagev1.CreateImageRequestResponse, error) { return okCreateImage() })

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	targets, expected, ready, failed, err := distributeRootfsArtifact(rootCtx,
		&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
		&types.CreateCubeSandboxReq{InstanceType: "cubebox"},
		readyDistributeArtifact("rfs-ok"), "tpl-ok", "job-ok")
	if err != nil {
		t.Fatalf("distributeRootfsArtifact() error = %v, want nil", err)
	}
	if len(targets) != 2 || expected != 2 || ready != 2 || failed != 0 {
		t.Fatalf("fan-out result = targets=%d expected=%d ready=%d failed=%d, want 2/2/2/0",
			len(targets), expected, ready, failed)
	}
	flush()

	span := requireSingleDistributeSpan(t, rec)
	if span.Status().Code == codes.Error {
		t.Errorf("distribute span status = %v, want non-error", span.Status().Code)
	}
	assertSpanStrAttr(t, span, telemetry.AttrJobID, "job-ok")
	assertSpanStrAttr(t, span, telemetry.AttrTemplateID, "tpl-ok")
	assertSpanStrAttr(t, span, telemetry.AttrArtifactID, "rfs-ok")
	if got := nodeImageChildCount(t, rec, span); got != 2 {
		t.Fatalf("node image spans = %d, want 2", got)
	}
}

func TestDistributeSpanFlagsFanOutBusinessFailure(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubDistributeFanOut(t, func(ip string) (*imagev1.CreateImageRequestResponse, error) {
		if ip == "10.0.0.2" {
			return &imagev1.CreateImageRequestResponse{
				Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Conflict, RetMsg: "busy"},
			}, nil
		}
		return okCreateImage()
	})

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	_, expected, ready, failed, err := distributeRootfsArtifact(rootCtx,
		&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
		&types.CreateCubeSandboxReq{InstanceType: "cubebox"},
		readyDistributeArtifact("rfs-partial"), "tpl-partial", "job-partial")
	if err == nil {
		t.Fatal("a per-node business failure must surface as a partial-failure error")
	}
	if expected != 2 || ready != 1 || failed != 1 {
		t.Fatalf("fan-out result = expected=%d ready=%d failed=%d, want 2/1/1", expected, ready, failed)
	}
	flush()

	span := requireSingleDistributeSpan(t, rec)
	if span.Status().Code != codes.Error {
		t.Errorf("distribute span status = %v, want Error when a node failed", span.Status().Code)
	}
}

func TestDistributeSpanOnExplicitTargetEntry(t *testing.T) {
	rec, flush := installSpanRecorder(t)
	stubDistributeFanOut(t, func(string) (*imagev1.CreateImageRequestResponse, error) { return okCreateImage() })

	rootCtx, rootSpan := telemetry.Start(context.Background(), "test.root")
	defer rootSpan.End()

	_, _, ready, failed, err := distributeRootfsArtifactToNodes(rootCtx,
		&types.CreateTemplateFromImageReq{InstanceType: "cubebox"},
		&types.CreateCubeSandboxReq{InstanceType: "cubebox"},
		readyDistributeArtifact("rfs-bf"), "tpl-bf", "job-bf",
		[]*node.Node{{InsID: "n1", IP: "10.0.0.1"}})
	if err != nil || ready != 1 || failed != 0 {
		t.Fatalf("explicit-target distribution = ready=%d failed=%d err=%v, want 1/0/nil", ready, failed, err)
	}
	flush()

	span := requireSingleDistributeSpan(t, rec)
	if span.Status().Code == codes.Error {
		t.Errorf("distribute span status = %v, want non-error", span.Status().Code)
	}
	if span.SpanContext().TraceID() != rootSpan.SpanContext().TraceID() {
		t.Error("distribute span did not continue the caller trace")
	}
	assertSpanStrAttr(t, span, telemetry.AttrJobID, "job-bf")
	assertSpanStrAttr(t, span, telemetry.AttrTemplateID, "tpl-bf")
	assertSpanStrAttr(t, span, telemetry.AttrArtifactID, "rfs-bf")
	if got := nodeImageChildCount(t, rec, span); got != 1 {
		t.Fatalf("node image spans = %d, want 1", got)
	}
}

func TestDistributeSpanAbsentWithoutTraceContext(t *testing.T) {
	tests := []struct {
		name          string
		explicitEntry bool
		targets       []*node.Node
		wantTargets   int
		wantReady     int32
	}{
		{
			name:        "resolve-entry",
			wantTargets: 2,
			wantReady:   2,
		},
		{
			name:          "explicit-target-entry",
			explicitEntry: true,
			targets:       []*node.Node{{InsID: "n1", IP: "10.0.0.1"}},
			wantTargets:   1,
			wantReady:     1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := installSpanRecorder(t)
			stubDistributeFanOut(t, func(string) (*imagev1.CreateImageRequestResponse, error) { return okCreateImage() })

			req := &types.CreateTemplateFromImageReq{InstanceType: "cubebox"}
			generatedReq := &types.CreateCubeSandboxReq{InstanceType: "cubebox"}
			artifact := readyDistributeArtifact("rfs-untraced")

			var targets []*node.Node
			var expected, ready, failed int32
			var err error
			if tc.explicitEntry {
				targets, expected, ready, failed, err = distributeRootfsArtifactToNodes(context.Background(),
					req, generatedReq, artifact, "tpl-untraced", "job-untraced", tc.targets)
			} else {
				targets, expected, ready, failed, err = distributeRootfsArtifact(context.Background(),
					req, generatedReq, artifact, "tpl-untraced", "job-untraced")
			}
			if err != nil {
				t.Fatalf("distribution without a trace context error = %v, want nil", err)
			}
			if len(targets) != tc.wantTargets || expected != int32(tc.wantTargets) || ready != tc.wantReady || failed != 0 {
				t.Fatalf("result = targets=%d expected=%d ready=%d failed=%d, want %d/%d/%d/0",
					len(targets), expected, ready, failed, tc.wantTargets, tc.wantTargets, tc.wantReady)
			}
			flush()
			if got := rec.snapshot(); len(got) != 0 {
				t.Fatalf("untraced distribution emitted %d spans, want 0", len(got))
			}
		})
	}
}
