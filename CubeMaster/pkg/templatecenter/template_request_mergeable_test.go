// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestGenerateTemplateCreateRequestMergeable(t *testing.T) {
	newReq := func() *types.CreateTemplateFromImageReq {
		return &types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-mergeable"},
			SourceImageRef:    "docker.io/library/nginx:latest",
			TemplateID:        "tpl-mergeable",
			WritableLayerSize: "1Gi",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			NetworkType:       cubeboxv1.NetworkType_tap.String(),
		}
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "rfs-mergeable",
		TemplateSpecFingerprint: "fp-mergeable",
		Ext4SHA256:              "sha-mergeable",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-mergeable",
	}

	yes := true
	req := newReq()
	req.Mergeable = &yes
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	if got.Annotations[constants.CubeAnnotationMemoryMergeable] != "true" {
		t.Fatalf("mergeable=true should stamp %s=true, got %q",
			constants.CubeAnnotationMemoryMergeable,
			got.Annotations[constants.CubeAnnotationMemoryMergeable])
	}

	no := false
	for _, mergeable := range []*bool{nil, &no} {
		req := newReq()
		req.Mergeable = mergeable
		got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "")
		if err != nil {
			t.Fatalf("generateTemplateCreateRequest failed: %v", err)
		}
		if _, ok := got.Annotations[constants.CubeAnnotationMemoryMergeable]; ok {
			t.Fatalf("mergeable=%v must not stamp the opt-in annotation", mergeable)
		}
	}
}

func TestGenerateTemplateCreateRequestStripsContainerMergeableAnnotations(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-container"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "tpl-container",
		WritableLayerSize: "1Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		ContainerOverrides: &types.ContainerOverrides{
			Annotations: map[string]string{
				constants.CubeAnnotationMemoryMergeable:        "true",
				constants.CubeAnnotationRuntimeMemoryMergeable: "true",
				"keep": "value",
			},
		},
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "rfs-container",
		TemplateSpecFingerprint: "fp-container",
		Ext4SHA256:              "sha-container",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-container",
	}

	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	if len(got.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(got.Containers))
	}
	for _, key := range []string{
		constants.CubeAnnotationMemoryMergeable,
		constants.CubeAnnotationRuntimeMemoryMergeable,
	} {
		if _, ok := got.Containers[0].Annotations[key]; ok {
			t.Fatalf("container annotation %s must be dropped", key)
		}
	}
	if got.Containers[0].Annotations["keep"] != "value" {
		t.Fatalf("unrelated container annotation was dropped: %v", got.Containers[0].Annotations)
	}
}
