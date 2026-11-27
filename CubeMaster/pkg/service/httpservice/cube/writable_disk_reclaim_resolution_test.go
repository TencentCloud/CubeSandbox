// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

const writableDiskReclaimAnnotationKeyTest = "cube.master.rootfs.writable_disk_reclaim"

// dealCubeboxCreateReqWithTemplateCenter is CubeMaster's second template
// resolution path (template kind -> applyTemplateAnnotationsAndLabels,
// snapshot kind -> applySnapshotTemplateAnnotationsAndLabels). Both must let
// the persisted template policy win and must not accept a caller-forged value
// for a template that predates the field.
func TestTemplateCenterCreatePathWritableDiskReclaimFollowsTemplatePolicy(t *testing.T) {
	for _, kind := range []string{templatecenter.TemplateKindTemplate, templatecenter.TemplateKindSnapshot} {
		for _, tc := range []struct {
			name            string
			templateValue   string
			templatePresent bool
			requestValue    string
			requestPresent  bool
			want            string
			allowReject     bool
		}{
			{
				name:          "template enables, request omits",
				templateValue: "true", templatePresent: true,
				want: "true",
			},
			{
				name:          "template enables, request tries to disable",
				templateValue: "true", templatePresent: true,
				requestValue: "false", requestPresent: true,
				want: "true", allowReject: true,
			},
			{
				name:          "template enables, request matches",
				templateValue: "true", templatePresent: true,
				requestValue: "true", requestPresent: true,
				want: "true",
			},
			{
				name: "legacy template missing policy stays off",
				want: "",
			},
			{
				name:         "legacy template missing policy rejects forged enable",
				requestValue: "true", requestPresent: true,
				want: "", allowReject: true,
			},
		} {
			kind, tc := kind, tc
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				patches := gomonkey.NewPatches()
				defer patches.Reset()

				templateAnnotations := map[string]string{}
				if tc.templatePresent {
					templateAnnotations[writableDiskReclaimAnnotationKeyTest] = tc.templateValue
				}
				patches.ApplyFunc(templatecenter.GetTemplateRequest, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
					return &types.CreateCubeSandboxReq{Backend: "xfs", Annotations: templateAnnotations}, nil
				})
				patches.ApplyFunc(templatecenter.GetTemplateKind, func(context.Context, string) (string, error) { return kind, nil })
				patches.ApplyFunc(templatecenter.EnsureTemplateLocalityReady, func(context.Context, string, string) error { return nil })

				oldSource, oldReady := getSnapshotRestoreSourceFn, ensureSnapshotReadyForNewUseFn
				oldSnapshot, oldTemplate := resolveSnapshotReadyReplicaFn, resolveTemplateReadyReplicaFn
				defer func() {
					getSnapshotRestoreSourceFn, ensureSnapshotReadyForNewUseFn = oldSource, oldReady
					resolveSnapshotReadyReplicaFn, resolveTemplateReadyReplicaFn = oldSnapshot, oldTemplate
				}()
				getSnapshotRestoreSourceFn = func(context.Context, string) (*templatecenter.RestoreSource, error) {
					return &templatecenter.RestoreSource{Backend: "xfs", OriginNodeID: "node-a", OriginNodeIP: "127.0.0.1"}, nil
				}
				ensureSnapshotReadyForNewUseFn = func(context.Context, string) error { return nil }
				replica := func(context.Context, string, string) (templatecenter.ReplicaStatus, error) {
					return templatecenter.ReplicaStatus{NodeID: "node-a"}, nil
				}
				resolveSnapshotReadyReplicaFn = replica
				resolveTemplateReadyReplicaFn = replica

				req := &types.CreateCubeSandboxReq{}
				if tc.requestPresent {
					req.Annotations = map[string]string{writableDiskReclaimAnnotationKeyTest: tc.requestValue}
				}

				err := dealCubeboxCreateReqWithTemplateCenter(context.Background(), "template-a", req)
				if err != nil {
					if !tc.allowReject || !strings.Contains(strings.ToLower(err.Error()), "writable") {
						t.Fatalf("unexpected error: %v", err)
					}
					return
				}
				got := req.Annotations[writableDiskReclaimAnnotationKeyTest]
				require.Equal(t, tc.want, got, "annotation %s", writableDiskReclaimAnnotationKeyTest)
			})
		}
	}
}
