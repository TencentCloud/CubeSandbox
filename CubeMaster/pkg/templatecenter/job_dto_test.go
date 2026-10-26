// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestCloneTemplateImageJobInfo(t *testing.T) {
	if cloneTemplateImageJobInfo(nil) != nil {
		t.Fatal("nil input should return nil")
	}

	src := &sandboxtypes.TemplateImageJobInfo{
		JobID:      "job-1",
		TemplateID: "tpl-1",
		Status:     JobStatusPending,
		RedoScope:  []string{"phase-a"},
	}
	dst := cloneTemplateImageJobInfo(src)
	if dst == nil || dst == src {
		t.Fatal("expected independent clone")
	}
	dst.RedoScope[0] = "changed"
	if src.RedoScope[0] == "changed" {
		t.Fatal("RedoScope should be deep-copied")
	}
}
