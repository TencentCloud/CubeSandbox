// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func TestNodeSafetyFilterRequiresSchedulerConfig(t *testing.T) {
	selection := selctx.New("random")
	selection.Ctx = context.Background()
	if _, err := NewNodeSafetyFilter().Select(selection); err == nil {
		t.Fatal("missing scheduler config must fail closed")
	}
}
