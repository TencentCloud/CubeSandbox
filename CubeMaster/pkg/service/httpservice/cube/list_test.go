// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestApplyListInstanceTypeDefault(t *testing.T) {
	t.Run("legacy request defaults to cubebox", func(t *testing.T) {
		req := &types.ListCubeSandboxReq{}
		applyListInstanceTypeDefault(req)
		if req.InstanceType != "cubebox" {
			t.Fatalf("InstanceType=%q, want cubebox", req.InstanceType)
		}
	})

	t.Run("all instance types remains unfiltered", func(t *testing.T) {
		req := &types.ListCubeSandboxReq{AllInstanceTypes: true}
		applyListInstanceTypeDefault(req)
		if req.InstanceType != "" {
			t.Fatalf("InstanceType=%q, want empty", req.InstanceType)
		}
	})
}
