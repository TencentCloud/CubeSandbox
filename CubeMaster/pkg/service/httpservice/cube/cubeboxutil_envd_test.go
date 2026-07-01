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

func TestStripPerRequestEnvdAnnotations(t *testing.T) {
	req := &types.CreateCubeSandboxReq{Annotations: map[string]string{
		constants.CubeAnnotationsInjectEnvd:       "true",
		constants.CubeAnnotationsEnvdSidecarImage: "image",
		"keep": "value",
	}}

	stripPerRequestEnvdAnnotations(context.Background(), req)

	assert.NotContains(t, req.Annotations, constants.CubeAnnotationsInjectEnvd)
	assert.NotContains(t, req.Annotations, constants.CubeAnnotationsEnvdSidecarImage)
	assert.Equal(t, "value", req.Annotations["keep"])
}
