// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func stripPerRequestEnvdAnnotations(ctx context.Context, req *types.CreateCubeSandboxReq) {
	if req == nil || len(req.Annotations) == 0 {
		return
	}
	stripped := make([]string, 0, 2)
	for _, key := range []string{
		constants.CubeAnnotationsInjectEnvd,
		constants.CubeAnnotationsEnvdSidecarImage,
	} {
		if _, ok := req.Annotations[key]; ok {
			delete(req.Annotations, key)
			stripped = append(stripped, key)
		}
	}
	if len(stripped) > 0 {
		log.G(ctx).Infof("envd-sidecar: stripped per-request annotation(s) %v; envd injection is template-only", stripped)
	}
}
