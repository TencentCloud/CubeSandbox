// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestStripUserMemoryMergeable(t *testing.T) {
	key := constants.CubeAnnotationMemoryMergeable
	runtimeKey := constants.CubeAnnotationRuntimeMemoryMergeable
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{key: "true", runtimeKey: "true", "keep-annotation": "a"},
		Labels:      map[string]string{key: "true", runtimeKey: "true", "keep-label": "b"},
		Containers: []*types.Container{{
			Annotations: map[string]string{key: "true", runtimeKey: "true", "keep-container": "c"},
			Image: &types.ImageSpec{
				Annotations: map[string]string{key: "true", runtimeKey: "true", "keep-image": "d"},
			},
		}},
	}

	stripUserMemoryMergeable(req)

	assert.NotContains(t, req.Annotations, key)
	assert.NotContains(t, req.Annotations, runtimeKey)
	assert.Equal(t, "a", req.Annotations["keep-annotation"])
	assert.NotContains(t, req.Labels, key)
	assert.NotContains(t, req.Labels, runtimeKey)
	assert.Equal(t, "b", req.Labels["keep-label"])
	assert.NotContains(t, req.Containers[0].Annotations, key)
	assert.NotContains(t, req.Containers[0].Annotations, runtimeKey)
	assert.Equal(t, "c", req.Containers[0].Annotations["keep-container"])
	assert.NotContains(t, req.Containers[0].Image.Annotations, key)
	assert.NotContains(t, req.Containers[0].Image.Annotations, runtimeKey)
	assert.Equal(t, "d", req.Containers[0].Image.Annotations["keep-image"])

	// Must stay safe when the request or its maps are absent.
	stripUserMemoryMergeable(nil)
	stripUserMemoryMergeable(&types.CreateCubeSandboxReq{
		Containers: []*types.Container{nil, {}},
	})
}

func TestDealCubeboxCreateReqWithTemplateStripsBeforeEarlyReturn(t *testing.T) {
	key := constants.CubeAnnotationMemoryMergeable
	runtimeKey := constants.CubeAnnotationRuntimeMemoryMergeable
	req := &types.CreateCubeSandboxReq{
		InstanceType: "not-cubebox",
		Annotations:  map[string]string{key: "true", runtimeKey: "true"},
		Labels:       map[string]string{key: "true", runtimeKey: "true"},
		Containers: []*types.Container{{
			Annotations: map[string]string{key: "true", runtimeKey: "true"},
			Image: &types.ImageSpec{
				Annotations: map[string]string{key: "true", runtimeKey: "true"},
			},
		}},
	}

	if err := dealCubeboxCreateReqWithTemplate(context.Background(), req); err != nil {
		t.Fatalf("dealCubeboxCreateReqWithTemplate: %v", err)
	}
	assert.NotContains(t, req.Annotations, key)
	assert.NotContains(t, req.Annotations, runtimeKey)
	assert.NotContains(t, req.Labels, key)
	assert.NotContains(t, req.Labels, runtimeKey)
	assert.NotContains(t, req.Containers[0].Annotations, key)
	assert.NotContains(t, req.Containers[0].Annotations, runtimeKey)
	assert.NotContains(t, req.Containers[0].Image.Annotations, key)
	assert.NotContains(t, req.Containers[0].Image.Annotations, runtimeKey)
}
